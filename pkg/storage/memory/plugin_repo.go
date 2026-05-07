package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MemoryRepository is a concurrent-safe in-memory Repository reference implementation.
type PluginRepository struct {
	mu          sync.RWMutex
	byID        map[plugin.PluginID]plugin.Plugin
	idByNameKey map[pluginNameIndexKey]plugin.PluginID
	closed      bool
}

type pluginNameIndexKey struct {
	namespace plugin.Namespace
	name      string
}

func NewPluginRepository() *PluginRepository {
	return &PluginRepository{
		byID:        make(map[plugin.PluginID]plugin.Plugin),
		idByNameKey: make(map[pluginNameIndexKey]plugin.PluginID),
	}
}

func (r *PluginRepository) Insert(_ context.Context, p plugin.Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return plugin.ErrStorageClosed
	}
	if existing, exists := r.byID[p.ID]; exists {
		if existing.Namespace == p.Namespace && existing.Name == p.Name {
			return errors.Join(plugin.ErrPluginIDConflict, plugin.ErrPluginNameConflict)
		}
		return plugin.ErrPluginIDConflict
	}
	nameKey := pluginNameKey(p.Namespace, p.Name)
	if _, exists := r.idByNameKey[nameKey]; exists {
		return plugin.ErrPluginNameConflict
	}

	r.byID[p.ID] = clonePlugin(p)
	r.idByNameKey[nameKey] = p.ID
	return nil
}

func (r *PluginRepository) Update(_ context.Context, p plugin.Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return plugin.ErrStorageClosed
	}

	existing, ok := r.byID[p.ID]
	if !ok {
		return plugin.ErrPluginNotFound
	}

	nameKey := pluginNameKey(p.Namespace, p.Name)
	if otherID, exists := r.idByNameKey[nameKey]; exists && otherID != p.ID {
		return plugin.ErrPluginNameConflict
	}

	// Keep immutable identity fields from the stored row.
	p.ID = existing.ID
	p.CreatedAt = existing.CreatedAt

	delete(r.idByNameKey, pluginNameKey(existing.Namespace, existing.Name))
	r.byID[p.ID] = clonePlugin(p)
	r.idByNameKey[nameKey] = p.ID
	return nil
}

func (r *PluginRepository) Get(_ context.Context, id plugin.PluginID) (plugin.Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return plugin.Plugin{}, plugin.ErrStorageClosed
	}

	p, ok := r.byID[id]
	if !ok {
		return plugin.Plugin{}, plugin.ErrPluginNotFound
	}
	return clonePlugin(p), nil
}

func (r *PluginRepository) GetByName(_ context.Context, namespace plugin.Namespace, name string) (plugin.Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return plugin.Plugin{}, plugin.ErrStorageClosed
	}

	id, ok := r.idByNameKey[pluginNameKey(namespace, name)]
	if !ok {
		return plugin.Plugin{}, plugin.ErrPluginNotFound
	}
	return clonePlugin(r.byID[id]), nil
}

func (r *PluginRepository) List(_ context.Context, opts plugin.ListOptions) ([]plugin.Plugin, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, "", plugin.ErrStorageClosed
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset, err := parsePluginCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}

	filtered := make([]plugin.Plugin, 0, len(r.byID))
	for _, p := range r.byID {
		if opts.Namespace != "" && p.Namespace != opts.Namespace {
			continue
		}
		if opts.NameLike != "" && !strings.Contains(p.Name, opts.NameLike) {
			continue
		}
		filtered = append(filtered, clonePlugin(p))
	}

	slices.SortFunc(filtered, func(a, b plugin.Plugin) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})

	if offset >= len(filtered) {
		return []plugin.Plugin{}, "", nil
	}

	remaining := len(filtered) - offset
	take := remaining
	if limit < take {
		take = limit
	}
	end := offset + take
	next := ""
	if end < len(filtered) {
		next = strconv.Itoa(end)
	}
	return filtered[offset:end], next, nil
}

func (r *PluginRepository) Delete(_ context.Context, id plugin.PluginID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return plugin.ErrStorageClosed
	}

	p, ok := r.byID[id]
	if !ok {
		return plugin.ErrPluginNotFound
	}

	delete(r.byID, id)
	delete(r.idByNameKey, pluginNameKey(p.Namespace, p.Name))
	return nil
}

func (r *PluginRepository) Close(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func pluginNameKey(namespace plugin.Namespace, name string) pluginNameIndexKey {
	return pluginNameIndexKey{
		namespace: namespace,
		name:      name,
	}
}

func parsePluginCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	return offset, nil
}

func clonePlugin(p plugin.Plugin) plugin.Plugin {
	p.Manifest = cloneManifest(p.Manifest)
	return p
}

func cloneManifest(m plugin.Manifest) plugin.Manifest {
	m.Plugin.Metadata = clonePluginMapAny(m.Plugin.Metadata)
	m.Skills = cloneManifestEntries(m.Skills)
	m.Rules = cloneManifestEntries(m.Rules)
	m.Hooks = cloneManifestEntries(m.Hooks)
	m.Subagents = cloneManifestEntries(m.Subagents)
	m.MCPs = cloneManifestEntries(m.MCPs)
	return m
}

func cloneManifestEntries(in map[string]plugin.ManifestEntry) map[string]plugin.ManifestEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]plugin.ManifestEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func clonePluginMapAny(in map[string]any) map[string]any {
	return clonePluginMapAnyDepth(in, 0)
}

func clonePluginMapAnyDepth(in map[string]any, depth int) map[string]any {
	if depth > 32 {
		return nil
	}
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepClonePluginAnyDepth(v, depth+1)
	}
	return out
}

func deepClonePluginAnyDepth(v any, depth int) any {
	if depth > 32 {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		return clonePluginMapAnyDepth(x, depth)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepClonePluginAnyDepth(x[i], depth+1)
		}
		return out
	default:
		return v
	}
}
