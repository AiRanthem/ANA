package memory

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
	"slices"
	"strings"
	"sync"
	"time"
)

var errRepositoryClosed = fmt.Errorf("workspace: repository closed")

// MemoryRepository is a concurrent-safe in-memory Repository implementation.
type WorkspaceRepository struct {
	mu           sync.RWMutex
	byID         map[workspace.WorkspaceID]workspace.Workspace
	idByAliasKey map[workspaceAliasIndexKey]workspace.WorkspaceID
	closed       bool
}

type workspaceAliasIndexKey struct {
	namespace workspace.Namespace
	alias     workspace.Alias
}

// NewWorkspaceRepository constructs an empty in-memory workspace repository.
func NewWorkspaceRepository() *WorkspaceRepository {
	return &WorkspaceRepository{
		byID:         make(map[workspace.WorkspaceID]workspace.Workspace),
		idByAliasKey: make(map[workspaceAliasIndexKey]workspace.WorkspaceID),
	}
}

func (r *WorkspaceRepository) Insert(_ context.Context, w workspace.Workspace) error {
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
		return workspace.ErrAliasConflict
	}

	r.byID[w.ID] = cloneWorkspace(w)
	r.idByAliasKey[aliasKey] = w.ID
	return nil
}

func (r *WorkspaceRepository) Get(_ context.Context, id workspace.WorkspaceID) (workspace.Workspace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return workspace.Workspace{}, errRepositoryClosed
	}
	row, ok := r.byID[id]
	if !ok {
		return workspace.Workspace{}, workspace.ErrWorkspaceNotFound
	}
	return cloneWorkspace(row), nil
}

func (r *WorkspaceRepository) GetByAlias(_ context.Context, namespace workspace.Namespace, alias workspace.Alias) (workspace.Workspace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return workspace.Workspace{}, errRepositoryClosed
	}

	id, ok := r.idByAliasKey[workspaceAliasKey(namespace, alias)]
	if !ok {
		return workspace.Workspace{}, workspace.ErrWorkspaceNotFound
	}
	return cloneWorkspace(r.byID[id]), nil
}

func (r *WorkspaceRepository) List(_ context.Context, opts workspace.ListOptions) ([]workspace.Workspace, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, "", errRepositoryClosed
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	cursorNamespace, cursorAlias, err := parseWorkspaceCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}

	filtered := make([]workspace.Workspace, 0, len(r.byID))
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

	slices.SortFunc(filtered, func(a, b workspace.Workspace) int {
		return compareNamespaceAlias(a.Namespace, a.Alias, b.Namespace, b.Alias)
	})

	if len(filtered) == 0 {
		return []workspace.Workspace{}, "", nil
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

func (r *WorkspaceRepository) Update(_ context.Context, w workspace.Workspace) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	existing, ok := r.byID[w.ID]
	if !ok {
		return workspace.ErrWorkspaceNotFound
	}
	if w.Status != existing.Status {
		return fmt.Errorf("%w: use UpdateStatus for %q -> %q", workspace.ErrInvalidStatusTransition, existing.Status, w.Status)
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

func (r *WorkspaceRepository) UpdateStatus(_ context.Context, id workspace.WorkspaceID, status workspace.Status, statusError *workspace.Error, lastProbeAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	row, ok := r.byID[id]
	if !ok {
		return workspace.ErrWorkspaceNotFound
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
	if status == workspace.StatusHealthy {
		row.StatusError = nil
	} else {
		row.StatusError = cloneError(statusError)
	}
	r.byID[id] = row
	return nil
}

func (r *WorkspaceRepository) UpdateStatusCAS(
	_ context.Context,
	id workspace.WorkspaceID,
	writer workspace.StatusWriter,
	expect workspace.Status,
	next workspace.Status,
	statusError *workspace.Error,
	lastProbeAt time.Time,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}
	row, ok := r.byID[id]
	if !ok {
		return workspace.ErrWorkspaceNotFound
	}
	if row.Status != expect {
		return fmt.Errorf("%w: workspace %q current=%q expect=%q",
			workspace.ErrStatusPreconditionFailed, id, row.Status, expect)
	}
	if err := validateKnownStatus(next); err != nil {
		return err
	}
	if err := validateTransitionByWriter(writer, expect, next); err != nil {
		return err
	}

	row.Status = next
	row.LastProbeAt = lastProbeAt
	row.UpdatedAt = time.Now().UTC()
	if next == workspace.StatusHealthy {
		row.StatusError = nil
	} else {
		row.StatusError = cloneError(statusError)
	}
	r.byID[id] = row
	return nil
}

func validateTransitionByWriter(writer workspace.StatusWriter, from, to workspace.Status) error {
	switch writer {
	case workspace.StatusWriterController:
		if from == workspace.StatusInit && (to == workspace.StatusHealthy || to == workspace.StatusFailed) {
			return nil
		}
	case workspace.StatusWriterScheduler:
		if from == workspace.StatusInit && to == workspace.StatusFailed {
			return nil
		}
		if from == workspace.StatusHealthy && to == workspace.StatusFailed {
			return nil
		}
		if from == workspace.StatusFailed && to == workspace.StatusHealthy {
			return nil
		}
	}
	return fmt.Errorf("%w: writer=%q %q->%q", workspace.ErrInvalidStatusTransition, writer, from, to)
}

func (r *WorkspaceRepository) Delete(_ context.Context, id workspace.WorkspaceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}

	row, ok := r.byID[id]
	if !ok {
		return workspace.ErrWorkspaceNotFound
	}

	delete(r.byID, id)
	delete(r.idByAliasKey, workspaceAliasKey(row.Namespace, row.Alias))
	return nil
}

func (r *WorkspaceRepository) Close(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func workspaceAliasKey(namespace workspace.Namespace, alias workspace.Alias) workspaceAliasIndexKey {
	return workspaceAliasIndexKey{
		namespace: namespace,
		alias:     alias,
	}
}

func encodeCursor(namespace workspace.Namespace, alias workspace.Alias) string {
	raw := string(namespace) + "\x00" + string(alias)
	return base64.RawStdEncoding.EncodeToString([]byte(raw))
}

func parseWorkspaceCursor(cursor string) (workspace.Namespace, workspace.Alias, error) {
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
	return workspace.Namespace(parts[0]), workspace.Alias(parts[1]), nil
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

func compareNamespaceAlias(leftNamespace workspace.Namespace, leftAlias workspace.Alias, rightNamespace workspace.Namespace, rightAlias workspace.Alias) int {
	if cmp := strings.Compare(string(leftNamespace), string(rightNamespace)); cmp != 0 {
		return cmp
	}
	return strings.Compare(string(leftAlias), string(rightAlias))
}

func validateKnownStatus(status workspace.Status) error {
	switch status {
	case workspace.StatusInit, workspace.StatusHealthy, workspace.StatusFailed:
		return nil
	default:
		return fmt.Errorf("%w: unknown status %q", workspace.ErrInvalidStatusTransition, status)
	}
}

func validateStatusTransition(from workspace.Status, to workspace.Status) error {
	switch {
	case from == workspace.StatusInit && to == workspace.StatusInit:
		return nil
	case from == workspace.StatusInit && (to == workspace.StatusHealthy || to == workspace.StatusFailed):
		return nil
	case from == workspace.StatusHealthy && to == workspace.StatusFailed:
		return nil
	case from == workspace.StatusFailed && to == workspace.StatusHealthy:
		return nil
	default:
		return fmt.Errorf("%w: %q -> %q", workspace.ErrInvalidStatusTransition, from, to)
	}
}

func validateImmutableFields(existing workspace.Workspace, updated workspace.Workspace) error {
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

func cloneWorkspace(w workspace.Workspace) workspace.Workspace {
	w.InfraOptions = cloneOptions(w.InfraOptions)
	w.InstallParams = cloneWorkspaceMapAny(w.InstallParams)
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
		out[k] = deepCloneWorkspaceAny(v)
	}
	return out
}

func cloneWorkspaceMapAny(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneWorkspaceAny(v)
	}
	return out
}

func cloneAttachedPlugins(in []workspace.AttachedPlugin) []workspace.AttachedPlugin {
	if len(in) == 0 {
		return nil
	}
	out := make([]workspace.AttachedPlugin, len(in))
	copy(out, in)
	for i := range out {
		out[i].PlacedPaths = slices.Clone(out[i].PlacedPaths)
	}
	return out
}

func cloneError(in *workspace.Error) *workspace.Error {
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

func deepCloneWorkspaceAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneWorkspaceMapAny(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneWorkspaceAny(x[i])
		}
		return out
	default:
		return v
	}
}
