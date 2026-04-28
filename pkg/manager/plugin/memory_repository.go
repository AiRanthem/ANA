package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MemoryRepository is a concurrent-safe in-memory Repository reference implementation.
type MemoryRepository struct {
	mu          sync.RWMutex
	byID        map[PluginID]Plugin
	idByNameKey map[string]PluginID
	closed      bool
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		byID:        make(map[PluginID]Plugin),
		idByNameKey: make(map[string]PluginID),
	}
}

func (r *MemoryRepository) Insert(_ context.Context, p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrStorageClosed
	}
	if existing, exists := r.byID[p.ID]; exists {
		if existing.Namespace == p.Namespace && existing.Name == p.Name {
			return errors.Join(ErrPluginIDConflict, ErrPluginNameConflict)
		}
		return ErrPluginIDConflict
	}
	nameKey := pluginNameKey(p.Namespace, p.Name)
	if _, exists := r.idByNameKey[nameKey]; exists {
		return ErrPluginNameConflict
	}

	r.byID[p.ID] = clonePlugin(p)
	r.idByNameKey[nameKey] = p.ID
	return nil
}

func (r *MemoryRepository) Update(_ context.Context, p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrStorageClosed
	}

	existing, ok := r.byID[p.ID]
	if !ok {
		return ErrPluginNotFound
	}

	nameKey := pluginNameKey(p.Namespace, p.Name)
	if otherID, exists := r.idByNameKey[nameKey]; exists && otherID != p.ID {
		return ErrPluginNameConflict
	}

	// Keep immutable identity fields from the stored row.
	p.ID = existing.ID
	p.CreatedAt = existing.CreatedAt

	delete(r.idByNameKey, pluginNameKey(existing.Namespace, existing.Name))
	r.byID[p.ID] = clonePlugin(p)
	r.idByNameKey[nameKey] = p.ID
	return nil
}

func (r *MemoryRepository) Get(_ context.Context, id PluginID) (Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return Plugin{}, ErrStorageClosed
	}

	p, ok := r.byID[id]
	if !ok {
		return Plugin{}, ErrPluginNotFound
	}
	return clonePlugin(p), nil
}

func (r *MemoryRepository) GetByName(_ context.Context, namespace Namespace, name string) (Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return Plugin{}, ErrStorageClosed
	}

	id, ok := r.idByNameKey[pluginNameKey(namespace, name)]
	if !ok {
		return Plugin{}, ErrPluginNotFound
	}
	return clonePlugin(r.byID[id]), nil
}

func (r *MemoryRepository) List(_ context.Context, opts ListOptions) ([]Plugin, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, "", ErrStorageClosed
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset, err := parseCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}

	filtered := make([]Plugin, 0, len(r.byID))
	for _, p := range r.byID {
		if opts.Namespace != "" && p.Namespace != opts.Namespace {
			continue
		}
		if opts.NameLike != "" && !strings.Contains(p.Name, opts.NameLike) {
			continue
		}
		filtered = append(filtered, clonePlugin(p))
	}

	slices.SortFunc(filtered, func(a, b Plugin) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})

	if offset >= len(filtered) {
		return []Plugin{}, "", nil
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

func (r *MemoryRepository) Delete(_ context.Context, id PluginID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrStorageClosed
	}

	p, ok := r.byID[id]
	if !ok {
		return ErrPluginNotFound
	}

	delete(r.byID, id)
	delete(r.idByNameKey, pluginNameKey(p.Namespace, p.Name))
	return nil
}

func (r *MemoryRepository) Close(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func pluginNameKey(namespace Namespace, name string) string {
	return string(namespace) + "/" + name
}

func parseCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	return offset, nil
}

func clonePlugin(p Plugin) Plugin {
	p.Manifest = cloneManifest(p.Manifest)
	return p
}

func cloneManifest(m Manifest) Manifest {
	m.Plugin.Metadata = cloneMapAny(m.Plugin.Metadata)
	m.Skills = cloneManifestEntries(m.Skills)
	m.Rules = cloneManifestEntries(m.Rules)
	m.Hooks = cloneManifestEntries(m.Hooks)
	m.Subagents = cloneManifestEntries(m.Subagents)
	m.MCPs = cloneManifestEntries(m.MCPs)
	return m
}

func cloneManifestEntries(in map[string]ManifestEntry) map[string]ManifestEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ManifestEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneMapAny(in map[string]any) map[string]any {
	return cloneMapAnyDepth(in, 0)
}

func cloneMapAnyDepth(in map[string]any, depth int) map[string]any {
	if depth > maxMetadataNestingDepth {
		return nil
	}
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAnyDepth(v, depth+1)
	}
	return out
}

func deepCloneAnyDepth(v any, depth int) any {
	if depth > maxMetadataNestingDepth {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		return cloneMapAnyDepth(x, depth)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneAnyDepth(x[i], depth+1)
		}
		return out
	default:
		return v
	}
}
