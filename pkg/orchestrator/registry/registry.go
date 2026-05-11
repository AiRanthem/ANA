package registry

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

type RuntimeKind string

const (
	RuntimeKindChatAPI       RuntimeKind = "chat_api"
	RuntimeKindResumableCLI  RuntimeKind = "resumable_cli"
	RuntimeKindSocketSession RuntimeKind = "socket_session"
)

var (
	ErrAliasNotFound      = errors.New("alias not found")
	ErrWorkspaceNotFound  = errors.New("workspace not found")
	ErrWorkspaceDisabled  = errors.New("workspace disabled")
	ErrAliasConflict      = errors.New("alias conflict")
	ErrInvalidWorkspace   = errors.New("invalid workspace")
	ErrNoDefaultWorkspace = errors.New("no default workspace")

	aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type Workspace struct {
	WorkspaceID    string
	Alias          string
	RuntimeType    string
	RuntimeKind    RuntimeKind
	Description    string
	Enabled        bool
	IsDefaultEntry bool
	RuntimeConfig  map[string]any
}

type AgentFactory func(ctx context.Context, ws Workspace) (agentio.Agent, error)

type Registry interface {
	Register(ctx context.Context, ws Workspace, factory AgentFactory) error
	Update(ctx context.Context, ws Workspace) error
	Disable(ctx context.Context, workspaceID string) error
	Enable(ctx context.Context, workspaceID string) error
	LookupByAlias(ctx context.Context, alias string) (Workspace, AgentFactory, error)
	LookupByID(ctx context.Context, workspaceID string) (Workspace, AgentFactory, error)
	Default(ctx context.Context) (Workspace, AgentFactory, error)
	List(ctx context.Context) ([]Workspace, error)
}

type MemoryRegistry struct {
	mu        sync.RWMutex
	byID      map[string]Workspace
	byAlias   map[string]string
	factories map[string]AgentFactory
	defaultID string
}

func NewMemory() *MemoryRegistry {
	return &MemoryRegistry{
		byID:      make(map[string]Workspace),
		byAlias:   make(map[string]string),
		factories: make(map[string]AgentFactory),
	}
}

func (r *MemoryRegistry) Register(ctx context.Context, ws Workspace, factory AgentFactory) error {
	_ = ctx

	if factory == nil {
		return fmt.Errorf("register workspace %q: %w", strings.TrimSpace(ws.WorkspaceID), ErrInvalidWorkspace)
	}

	normalized, err := validateWorkspace(ws)
	if err != nil {
		return fmt.Errorf("register workspace %q: %w", strings.TrimSpace(ws.WorkspaceID), err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byID[normalized.WorkspaceID]; exists {
		return fmt.Errorf("register workspace %q: duplicate workspace id: %w", normalized.WorkspaceID, ErrInvalidWorkspace)
	}
	if existingID, exists := r.byAlias[normalized.Alias]; exists {
		return fmt.Errorf("register alias %q for workspace %q conflicts with workspace %q: %w", normalized.Alias, normalized.WorkspaceID, existingID, ErrAliasConflict)
	}

	normalized.RuntimeConfig = cloneRuntimeConfig(normalized.RuntimeConfig)
	storedFactory := wrapFactory(factory)

	if normalized.IsDefaultEntry && r.defaultID != "" && r.defaultID != normalized.WorkspaceID {
		currentDefault := r.byID[r.defaultID]
		currentDefault.IsDefaultEntry = false
		r.byID[r.defaultID] = currentDefault
	}

	r.byID[normalized.WorkspaceID] = normalized
	r.byAlias[normalized.Alias] = normalized.WorkspaceID
	r.factories[normalized.WorkspaceID] = storedFactory
	if normalized.IsDefaultEntry {
		r.defaultID = normalized.WorkspaceID
	}

	return nil
}

func (r *MemoryRegistry) Update(ctx context.Context, ws Workspace) error {
	_ = ctx

	normalized, err := validateWorkspace(ws)
	if err != nil {
		return fmt.Errorf("update workspace %q: %w", strings.TrimSpace(ws.WorkspaceID), err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, exists := r.byID[normalized.WorkspaceID]
	if !exists {
		return fmt.Errorf("update workspace %q: %w", normalized.WorkspaceID, ErrWorkspaceNotFound)
	}
	if normalized.Alias != existing.Alias {
		return fmt.Errorf("update workspace %q alias %q: %w", normalized.WorkspaceID, normalized.Alias, ErrInvalidWorkspace)
	}

	normalized.RuntimeConfig = cloneRuntimeConfig(normalized.RuntimeConfig)

	switch {
	case normalized.IsDefaultEntry && r.defaultID != "" && r.defaultID != normalized.WorkspaceID:
		currentDefault := r.byID[r.defaultID]
		currentDefault.IsDefaultEntry = false
		r.byID[r.defaultID] = currentDefault
		r.defaultID = normalized.WorkspaceID
	case normalized.IsDefaultEntry:
		r.defaultID = normalized.WorkspaceID
	case r.defaultID == normalized.WorkspaceID:
		r.defaultID = ""
	}

	r.byID[normalized.WorkspaceID] = normalized
	return nil
}

func (r *MemoryRegistry) Disable(ctx context.Context, workspaceID string) error {
	_ = ctx

	trimmedID := strings.TrimSpace(workspaceID)

	r.mu.Lock()
	defer r.mu.Unlock()

	ws, exists := r.byID[trimmedID]
	if !exists {
		return fmt.Errorf("disable workspace %q: %w", trimmedID, ErrWorkspaceNotFound)
	}
	if !ws.Enabled {
		return nil
	}

	ws.Enabled = false
	r.byID[trimmedID] = ws
	return nil
}

func (r *MemoryRegistry) Enable(ctx context.Context, workspaceID string) error {
	_ = ctx

	trimmedID := strings.TrimSpace(workspaceID)

	r.mu.Lock()
	defer r.mu.Unlock()

	ws, exists := r.byID[trimmedID]
	if !exists {
		return fmt.Errorf("enable workspace %q: %w", trimmedID, ErrWorkspaceNotFound)
	}
	if ws.Enabled {
		return nil
	}

	ws.Enabled = true
	r.byID[trimmedID] = ws
	return nil
}

func (r *MemoryRegistry) LookupByAlias(ctx context.Context, alias string) (Workspace, AgentFactory, error) {
	_ = ctx

	trimmedAlias := strings.TrimSpace(alias)

	r.mu.RLock()
	defer r.mu.RUnlock()

	workspaceID, exists := r.byAlias[trimmedAlias]
	if !exists {
		return Workspace{}, nil, fmt.Errorf("lookup alias %q: %w", trimmedAlias, ErrAliasNotFound)
	}

	return r.lookupByIDLocked(workspaceID)
}

func (r *MemoryRegistry) LookupByID(ctx context.Context, workspaceID string) (Workspace, AgentFactory, error) {
	_ = ctx

	trimmedID := strings.TrimSpace(workspaceID)

	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.lookupByIDLocked(trimmedID)
}

func (r *MemoryRegistry) Default(ctx context.Context) (Workspace, AgentFactory, error) {
	_ = ctx

	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.defaultID == "" {
		return Workspace{}, nil, ErrNoDefaultWorkspace
	}

	return r.lookupByIDLocked(r.defaultID)
}

func (r *MemoryRegistry) List(ctx context.Context) ([]Workspace, error) {
	_ = ctx

	r.mu.RLock()
	defer r.mu.RUnlock()

	workspaces := make([]Workspace, 0, len(r.byID))
	for _, ws := range r.byID {
		workspaces = append(workspaces, cloneWorkspace(ws))
	}

	sort.Slice(workspaces, func(i, j int) bool {
		return workspaces[i].Alias < workspaces[j].Alias
	})

	return workspaces, nil
}

func (r *MemoryRegistry) lookupByIDLocked(workspaceID string) (Workspace, AgentFactory, error) {
	ws, exists := r.byID[workspaceID]
	if !exists {
		return Workspace{}, nil, fmt.Errorf("lookup workspace %q: %w", workspaceID, ErrWorkspaceNotFound)
	}
	if !ws.Enabled {
		return Workspace{}, nil, fmt.Errorf("lookup workspace %q: %w", workspaceID, ErrWorkspaceDisabled)
	}

	factory, exists := r.factories[workspaceID]
	if !exists {
		return Workspace{}, nil, fmt.Errorf("lookup workspace %q factory: %w", workspaceID, ErrInvalidWorkspace)
	}

	return cloneWorkspace(ws), factory, nil
}

func validateWorkspace(ws Workspace) (Workspace, error) {
	normalized := ws
	normalized.WorkspaceID = strings.TrimSpace(ws.WorkspaceID)
	if normalized.WorkspaceID == "" {
		return Workspace{}, ErrInvalidWorkspace
	}

	normalized.Alias = strings.TrimSpace(ws.Alias)
	if normalized.Alias == "" {
		return Workspace{}, ErrInvalidWorkspace
	}
	if len(normalized.Alias) > 64 {
		return Workspace{}, ErrInvalidWorkspace
	}
	if !aliasPattern.MatchString(normalized.Alias) {
		return Workspace{}, ErrInvalidWorkspace
	}

	normalized.RuntimeType = strings.TrimSpace(ws.RuntimeType)
	if normalized.RuntimeType == "" {
		return Workspace{}, ErrInvalidWorkspace
	}

	switch normalized.RuntimeKind {
	case RuntimeKindChatAPI, RuntimeKindResumableCLI, RuntimeKindSocketSession:
	default:
		return Workspace{}, ErrInvalidWorkspace
	}

	if utf8.RuneCountInString(normalized.Description) > 256 {
		return Workspace{}, ErrInvalidWorkspace
	}

	if hasRuntimeConfigCycle(normalized.RuntimeConfig) {
		return Workspace{}, ErrInvalidWorkspace
	}

	normalized.RuntimeConfig = cloneRuntimeConfig(ws.RuntimeConfig)
	return normalized, nil
}

func wrapFactory(factory AgentFactory) AgentFactory {
	return func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		agent, err := factory(ctx, cloneWorkspace(ws))
		if err != nil {
			return nil, err
		}
		if agent == nil {
			return nil, fmt.Errorf("agent factory for workspace %q: %w", ws.WorkspaceID, ErrInvalidWorkspace)
		}
		return agent, nil
	}
}

func cloneWorkspace(ws Workspace) Workspace {
	clone := ws
	clone.RuntimeConfig = cloneRuntimeConfig(ws.RuntimeConfig)
	return clone
}

func cloneRuntimeConfig(cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}

	clone := make(map[string]any, len(cfg))
	for key, value := range cfg {
		clone[key] = cloneRuntimeValue(value)
	}
	return clone
}

func cloneRuntimeValue(value any) any {
	if value == nil {
		return nil
	}

	return cloneReflectValue(reflect.ValueOf(value)).Interface()
}

func cloneReflectValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}

		cloned := cloneReflectValue(value.Elem())
		wrapped := reflect.New(value.Type()).Elem()
		wrapped.Set(cloned)
		return wrapped
	case reflect.Ptr:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}

		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneReflectValue(value.Elem()))
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}

		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			cloned.SetMapIndex(iter.Key(), cloneReflectValue(iter.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}

		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		for i := range value.NumField() {
			field := cloned.Field(i)
			if !field.CanSet() {
				continue
			}
			field.Set(cloneReflectValue(value.Field(i)))
		}
		return cloned
	default:
		return value
	}
}

type runtimeValueRef struct {
	typ  reflect.Type
	kind reflect.Kind
	ptr  uintptr
}

func hasRuntimeConfigCycle(cfg map[string]any) bool {
	if cfg == nil {
		return false
	}

	return detectRuntimeConfigCycle(reflect.ValueOf(cfg), make(map[runtimeValueRef]struct{}), make(map[runtimeValueRef]struct{}))
}

func detectRuntimeConfigCycle(value reflect.Value, visiting, visited map[runtimeValueRef]struct{}) bool {
	if !value.IsValid() {
		return false
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return false
		}
		return detectRuntimeConfigCycle(value.Elem(), visiting, visited)
	case reflect.Ptr:
		if value.IsNil() {
			return false
		}
		ref := runtimeValueRef{typ: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if _, ok := visiting[ref]; ok {
			return true
		}
		if _, ok := visited[ref]; ok {
			return false
		}
		visiting[ref] = struct{}{}
		defer func() {
			delete(visiting, ref)
			visited[ref] = struct{}{}
		}()
		return detectRuntimeConfigCycle(value.Elem(), visiting, visited)
	case reflect.Map:
		if value.IsNil() {
			return false
		}
		ref := runtimeValueRef{typ: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if _, ok := visiting[ref]; ok {
			return true
		}
		if _, ok := visited[ref]; ok {
			return false
		}
		visiting[ref] = struct{}{}
		defer func() {
			delete(visiting, ref)
			visited[ref] = struct{}{}
		}()
		iter := value.MapRange()
		for iter.Next() {
			if detectRuntimeConfigCycle(iter.Key(), visiting, visited) {
				return true
			}
			if detectRuntimeConfigCycle(iter.Value(), visiting, visited) {
				return true
			}
		}
		return false
	case reflect.Slice:
		if value.IsNil() {
			return false
		}
		ref := runtimeValueRef{typ: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if _, ok := visiting[ref]; ok {
			return true
		}
		if _, ok := visited[ref]; ok {
			return false
		}
		visiting[ref] = struct{}{}
		defer func() {
			delete(visiting, ref)
			visited[ref] = struct{}{}
		}()
		for i := range value.Len() {
			if detectRuntimeConfigCycle(value.Index(i), visiting, visited) {
				return true
			}
		}
		return false
	case reflect.Array:
		for i := range value.Len() {
			if detectRuntimeConfigCycle(value.Index(i), visiting, visited) {
				return true
			}
		}
		return false
	case reflect.Struct:
		for i := range value.NumField() {
			if detectRuntimeConfigCycle(value.Field(i), visiting, visited) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
