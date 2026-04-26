package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

var errRepositoryClosed = fmt.Errorf("workspace: repository closed")

// MemoryRepository is a concurrent-safe in-memory Repository implementation.
type MemoryRepository struct {
	mu           sync.RWMutex
	byID         map[WorkspaceID]Workspace
	idByAliasKey map[string]WorkspaceID
	closed       bool
}

// NewMemoryRepository constructs an empty in-memory workspace repository.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		byID:         make(map[WorkspaceID]Workspace),
		idByAliasKey: make(map[string]WorkspaceID),
	}
}

func (r *MemoryRepository) Insert(_ context.Context, w Workspace) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}
	if _, ok := r.byID[w.ID]; ok {
		return fmt.Errorf("workspace insert %q: duplicate id", w.ID)
	}
	if err := validateKnownStatus(w.Status); err != nil {
		return err
	}

	aliasKey := workspaceAliasKey(w.Namespace, w.Alias)
	if _, ok := r.idByAliasKey[aliasKey]; ok {
		return ErrAliasConflict
	}

	r.byID[w.ID] = cloneWorkspace(w)
	r.idByAliasKey[aliasKey] = w.ID
	return nil
}

func (r *MemoryRepository) Get(_ context.Context, id WorkspaceID) (Workspace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return Workspace{}, errRepositoryClosed
	}
	row, ok := r.byID[id]
	if !ok {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return cloneWorkspace(row), nil
}

func (r *MemoryRepository) GetByAlias(_ context.Context, namespace Namespace, alias Alias) (Workspace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return Workspace{}, errRepositoryClosed
	}

	id, ok := r.idByAliasKey[workspaceAliasKey(namespace, alias)]
	if !ok {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return cloneWorkspace(r.byID[id]), nil
}

func (r *MemoryRepository) List(_ context.Context, opts ListOptions) ([]Workspace, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, "", errRepositoryClosed
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	cursorNamespace, cursorAlias, err := parseCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}

	filtered := make([]Workspace, 0, len(r.byID))
	for _, row := range r.byID {
		if opts.Namespace != "" && row.Namespace != opts.Namespace {
			continue
		}
		if opts.AgentType != "" && row.AgentType != opts.AgentType {
			continue
		}
		if opts.InfraType != "" && row.InfraType != opts.InfraType {
			continue
		}
		if opts.Status != "" && row.Status != opts.Status {
			continue
		}
		if !labelsMatch(row.Labels, opts.Labels) {
			continue
		}
		if opts.Cursor != "" && compareNamespaceAlias(row.Namespace, row.Alias, cursorNamespace, cursorAlias) <= 0 {
			continue
		}
		filtered = append(filtered, cloneWorkspace(row))
	}

	slices.SortFunc(filtered, func(a, b Workspace) int {
		return compareNamespaceAlias(a.Namespace, a.Alias, b.Namespace, b.Alias)
	})

	if len(filtered) == 0 {
		return []Workspace{}, "", nil
	}

	end := min(limit, len(filtered))
	rows := filtered[:end]
	next := ""
	if end < len(filtered) {
		last := rows[len(rows)-1]
		next = encodeCursor(last.Namespace, last.Alias)
	}
	return rows, next, nil
}

func (r *MemoryRepository) Update(_ context.Context, w Workspace) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	existing, ok := r.byID[w.ID]
	if !ok {
		return ErrWorkspaceNotFound
	}
	if w.Status != existing.Status {
		return fmt.Errorf("%w: use UpdateStatus for %q -> %q", ErrInvalidStatusTransition, existing.Status, w.Status)
	}
	if err := validateKnownStatus(w.Status); err != nil {
		return err
	}
	if err := validateImmutableFields(existing, w); err != nil {
		return err
	}

	w.ID = existing.ID
	w.Namespace = existing.Namespace
	w.Alias = existing.Alias
	w.AgentType = existing.AgentType
	w.InfraType = existing.InfraType
	w.CreatedAt = existing.CreatedAt

	r.byID[w.ID] = cloneWorkspace(w)
	return nil
}

func (r *MemoryRepository) UpdateStatus(_ context.Context, id WorkspaceID, status Status, statusError *Error, lastProbeAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	row, ok := r.byID[id]
	if !ok {
		return ErrWorkspaceNotFound
	}
	if err := validateKnownStatus(status); err != nil {
		return err
	}
	if err := validateStatusTransition(row.Status, status); err != nil {
		return err
	}

	row.Status = status
	row.LastProbeAt = lastProbeAt
	row.UpdatedAt = time.Now().UTC()
	if status == StatusHealthy {
		row.StatusError = nil
	} else {
		row.StatusError = cloneError(statusError)
	}
	r.byID[id] = row
	return nil
}

func (r *MemoryRepository) Delete(_ context.Context, id WorkspaceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	row, ok := r.byID[id]
	if !ok {
		return ErrWorkspaceNotFound
	}

	delete(r.byID, id)
	delete(r.idByAliasKey, workspaceAliasKey(row.Namespace, row.Alias))
	return nil
}

func (r *MemoryRepository) Close(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func workspaceAliasKey(namespace Namespace, alias Alias) string {
	return string(namespace) + "\x00" + string(alias)
}

func encodeCursor(namespace Namespace, alias Alias) string {
	raw := string(namespace) + "\x00" + string(alias)
	return base64.RawStdEncoding.EncodeToString([]byte(raw))
}

func parseCursor(cursor string) (Namespace, Alias, error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", fmt.Errorf("workspace: invalid cursor %q", cursor)
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("workspace: invalid cursor %q", cursor)
	}
	return Namespace(parts[0]), Alias(parts[1]), nil
}

func labelsMatch(rowLabels, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	for k, v := range want {
		if rowLabels[k] != v {
			return false
		}
	}
	return true
}

func compareNamespaceAlias(leftNamespace Namespace, leftAlias Alias, rightNamespace Namespace, rightAlias Alias) int {
	if cmp := strings.Compare(string(leftNamespace), string(rightNamespace)); cmp != 0 {
		return cmp
	}
	return strings.Compare(string(leftAlias), string(rightAlias))
}

func validateKnownStatus(status Status) error {
	switch status {
	case StatusInit, StatusHealthy, StatusFailed:
		return nil
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidStatusTransition, status)
	}
}

func validateStatusTransition(from Status, to Status) error {
	switch {
	case from == StatusInit && to == StatusInit:
		return nil
	case from == StatusInit && (to == StatusHealthy || to == StatusFailed):
		return nil
	case from == StatusHealthy && to == StatusFailed:
		return nil
	case from == StatusFailed && to == StatusHealthy:
		return nil
	default:
		return fmt.Errorf("%w: %q -> %q", ErrInvalidStatusTransition, from, to)
	}
}

func validateImmutableFields(existing Workspace, updated Workspace) error {
	if existing.Namespace != updated.Namespace {
		return fmt.Errorf("workspace update %q: namespace is immutable", existing.ID)
	}
	if existing.Alias != updated.Alias {
		return fmt.Errorf("workspace update %q: alias is immutable", existing.ID)
	}
	if existing.AgentType != updated.AgentType {
		return fmt.Errorf("workspace update %q: agent_type is immutable", existing.ID)
	}
	if existing.InfraType != updated.InfraType {
		return fmt.Errorf("workspace update %q: infra_type is immutable", existing.ID)
	}
	return nil
}

func cloneWorkspace(w Workspace) Workspace {
	w.InfraOptions = cloneOptions(w.InfraOptions)
	w.InstallParams = cloneMapAny(w.InstallParams)
	w.Plugins = cloneAttachedPlugins(w.Plugins)
	w.StatusError = cloneError(w.StatusError)
	w.Labels = cloneLabels(w.Labels)
	return w
}

func cloneOptions(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAny(v)
	}
	return out
}

func cloneMapAny(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAny(v)
	}
	return out
}

func cloneAttachedPlugins(in []AttachedPlugin) []AttachedPlugin {
	if len(in) == 0 {
		return nil
	}
	out := make([]AttachedPlugin, len(in))
	copy(out, in)
	for i := range out {
		out[i].PlacedPaths = slices.Clone(out[i].PlacedPaths)
	}
	return out
}

func cloneError(in *Error) *Error {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func deepCloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMapAny(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneAny(x[i])
		}
		return out
	default:
		return v
	}
}
