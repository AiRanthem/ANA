package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

func TestMemoryRegistry_RegisterAndLookupByID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-alpha", "alpha")

	if err := reg.Register(ctx, ws, testFactory("alpha-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, factory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if factory == nil {
		t.Fatal("LookupByID() factory = nil")
	}
	if !reflect.DeepEqual(got, ws) {
		t.Fatalf("LookupByID() workspace mismatch: got %#v want %#v", got, ws)
	}

	got.RuntimeConfig["path"] = "/tmp/changed"

	again, againFactory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("second LookupByID() error = %v", err)
	}
	if againFactory == nil {
		t.Fatal("second LookupByID() factory = nil")
	}
	if again.RuntimeConfig["path"] != "/tmp/agent" {
		t.Fatalf("RuntimeConfig clone was not preserved: got %v", again.RuntimeConfig["path"])
	}
}

func TestMemoryRegistry_LookupByAlias(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-bravo", "bravo")

	if err := reg.Register(ctx, ws, testFactory("bravo-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, factory, err := reg.LookupByAlias(ctx, ws.Alias)
	if err != nil {
		t.Fatalf("LookupByAlias() error = %v", err)
	}
	if factory == nil {
		t.Fatal("LookupByAlias() factory = nil")
	}
	if got.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("LookupByAlias() WorkspaceID = %q want %q", got.WorkspaceID, ws.WorkspaceID)
	}
}

func TestMemoryRegistry_AliasCollisionLeavesStateUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	original := testWorkspace("ws-charlie", "shared")

	if err := reg.Register(ctx, original, testFactory("charlie-agent")); err != nil {
		t.Fatalf("Register(original) error = %v", err)
	}

	collision := testWorkspace("ws-delta", "shared")
	err := reg.Register(ctx, collision, testFactory("delta-agent"))
	if !errors.Is(err, ErrAliasConflict) {
		t.Fatalf("Register(collision) error = %v, want ErrAliasConflict", err)
	}

	got, _, err := reg.LookupByAlias(ctx, original.Alias)
	if err != nil {
		t.Fatalf("LookupByAlias(original) error = %v", err)
	}
	if got.WorkspaceID != original.WorkspaceID {
		t.Fatalf("original alias remapped to %q want %q", got.WorkspaceID, original.WorkspaceID)
	}

	_, _, err = reg.LookupByID(ctx, collision.WorkspaceID)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("LookupByID(collision) error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestMemoryRegistry_DisabledWorkspaceLookup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-echo", "echo")

	if err := reg.Register(ctx, ws, testFactory("echo-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := reg.Disable(ctx, ws.WorkspaceID); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}

	_, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if !errors.Is(err, ErrWorkspaceDisabled) {
		t.Fatalf("LookupByID() error = %v, want ErrWorkspaceDisabled", err)
	}

	_, _, err = reg.LookupByAlias(ctx, ws.Alias)
	if !errors.Is(err, ErrWorkspaceDisabled) {
		t.Fatalf("LookupByAlias() error = %v, want ErrWorkspaceDisabled", err)
	}
}

func TestMemoryRegistry_ReenableRestoresLookups(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-echo-reenable", "echo-reenable")

	if err := reg.Register(ctx, ws, testFactory("echo-reenable-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := reg.Disable(ctx, ws.WorkspaceID); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}

	_, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if !errors.Is(err, ErrWorkspaceDisabled) {
		t.Fatalf("LookupByID() after Disable error = %v, want ErrWorkspaceDisabled", err)
	}

	if err := reg.Enable(ctx, ws.WorkspaceID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	gotByID, factoryByID, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() after Enable error = %v", err)
	}
	if factoryByID == nil {
		t.Fatal("LookupByID() after Enable factory = nil")
	}
	if gotByID.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("LookupByID() after Enable WorkspaceID = %q want %q", gotByID.WorkspaceID, ws.WorkspaceID)
	}

	gotByAlias, factoryByAlias, err := reg.LookupByAlias(ctx, ws.Alias)
	if err != nil {
		t.Fatalf("LookupByAlias() after Enable error = %v", err)
	}
	if factoryByAlias == nil {
		t.Fatal("LookupByAlias() after Enable factory = nil")
	}
	if gotByAlias.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("LookupByAlias() after Enable WorkspaceID = %q want %q", gotByAlias.WorkspaceID, ws.WorkspaceID)
	}
}

func TestMemoryRegistry_DefaultWorkspace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-foxtrot", "foxtrot")
	ws.IsDefaultEntry = true

	if err := reg.Register(ctx, ws, testFactory("foxtrot-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, factory, err := reg.Default(ctx)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if factory == nil {
		t.Fatal("Default() factory = nil")
	}
	if !reflect.DeepEqual(got, ws) {
		t.Fatalf("Default() workspace mismatch: got %#v want %#v", got, ws)
	}
}

func TestMemoryRegistry_MultipleDefaultRegistrationsDemotePrevious(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	first := testWorkspace("ws-golf", "golf")
	first.IsDefaultEntry = true
	second := testWorkspace("ws-hotel", "hotel")
	second.IsDefaultEntry = true

	if err := reg.Register(ctx, first, testFactory("golf-agent")); err != nil {
		t.Fatalf("Register(first) error = %v", err)
	}
	if err := reg.Register(ctx, second, testFactory("hotel-agent")); err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}

	gotDefault, _, err := reg.Default(ctx)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if gotDefault.WorkspaceID != second.WorkspaceID {
		t.Fatalf("Default() WorkspaceID = %q want %q", gotDefault.WorkspaceID, second.WorkspaceID)
	}

	gotFirst, _, err := reg.LookupByID(ctx, first.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID(first) error = %v", err)
	}
	if gotFirst.IsDefaultEntry {
		t.Fatal("previous default remained marked as default")
	}
}

func TestMemoryRegistry_ConcurrentRegisterAndLookup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()

	const workers = 24

	var wg sync.WaitGroup
	errCh := make(chan error, workers*2)

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			ws := testWorkspace(fmt.Sprintf("ws-%02d", i), fmt.Sprintf("alias-%02d", i))
			if err := reg.Register(ctx, ws, testFactory(ws.Alias)); err != nil {
				errCh <- fmt.Errorf("register %d: %w", i, err)
				return
			}

			gotByID, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
			if err != nil {
				errCh <- fmt.Errorf("lookup by id %d: %w", i, err)
				return
			}
			if gotByID.Alias != ws.Alias {
				errCh <- fmt.Errorf("lookup by id %d alias = %q want %q", i, gotByID.Alias, ws.Alias)
				return
			}

			gotByAlias, _, err := reg.LookupByAlias(ctx, ws.Alias)
			if err != nil {
				errCh <- fmt.Errorf("lookup by alias %d: %w", i, err)
				return
			}
			if gotByAlias.WorkspaceID != ws.WorkspaceID {
				errCh <- fmt.Errorf("lookup by alias %d id = %q want %q", i, gotByAlias.WorkspaceID, ws.WorkspaceID)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryRegistry_FactoryErrorSurfacesUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-kilo", "kilo")
	factoryErr := errors.New("factory failed")

	if err := reg.Register(ctx, ws, func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		return nil, factoryErr
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, factory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if factory == nil {
		t.Fatal("LookupByID() factory = nil")
	}

	agent, err := factory(ctx, ws)
	if !errors.Is(err, factoryErr) {
		t.Fatalf("factory() error = %v, want wrapped factoryErr", err)
	}
	if agent != nil {
		t.Fatalf("factory() agent = %#v want nil", agent)
	}
}

func TestMemoryRegistry_FailedRegistrationLeavesRegistryUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	original := testWorkspace("ws-india", "india")
	original.IsDefaultEntry = true

	if err := reg.Register(ctx, original, testFactory("india-agent")); err != nil {
		t.Fatalf("Register(original) error = %v", err)
	}

	collision := testWorkspace("ws-juliet", "india")
	collision.IsDefaultEntry = true

	err := reg.Register(ctx, collision, testFactory("juliet-agent"))
	if !errors.Is(err, ErrAliasConflict) {
		t.Fatalf("Register(collision) error = %v, want ErrAliasConflict", err)
	}

	gotDefault, _, err := reg.Default(ctx)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if gotDefault.WorkspaceID != original.WorkspaceID {
		t.Fatalf("Default() WorkspaceID = %q want %q", gotDefault.WorkspaceID, original.WorkspaceID)
	}

	gotOriginal, _, err := reg.LookupByID(ctx, original.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID(original) error = %v", err)
	}
	if !gotOriginal.IsDefaultEntry {
		t.Fatal("original workspace lost default flag after failed registration")
	}

	_, _, err = reg.LookupByID(ctx, collision.WorkspaceID)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("LookupByID(collision) error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestMemoryRegistry_RegisterRejectsOversizedDescription(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-lima", "lima")
	ws.Description = strings.Repeat("x", 257)

	err := reg.Register(ctx, ws, testFactory("lima-agent"))
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("Register() error = %v, want ErrInvalidWorkspace", err)
	}

	_, _, err = reg.LookupByAlias(ctx, ws.Alias)
	if !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("LookupByAlias() error = %v, want ErrAliasNotFound", err)
	}

	_, _, err = reg.LookupByID(ctx, ws.WorkspaceID)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("LookupByID() error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestMemoryRegistry_RegisterRejectsBadAlias(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()

	for _, alias := range []string{"bad alias", "bad#alias"} {
		t.Run(alias, func(t *testing.T) {
			ws := testWorkspace("ws-"+alias, alias)

			err := reg.Register(ctx, ws, testFactory("bad-alias-agent"))
			if !errors.Is(err, ErrInvalidWorkspace) {
				t.Fatalf("Register() error = %v, want ErrInvalidWorkspace", err)
			}

			_, _, err = reg.LookupByAlias(ctx, alias)
			if !errors.Is(err, ErrAliasNotFound) {
				t.Fatalf("LookupByAlias() error = %v, want ErrAliasNotFound", err)
			}

			_, _, err = reg.LookupByID(ctx, ws.WorkspaceID)
			if !errors.Is(err, ErrWorkspaceNotFound) {
				t.Fatalf("LookupByID() error = %v, want ErrWorkspaceNotFound", err)
			}
		})
	}
}

func TestMemoryRegistry_FactoryWrapperRejectsNilAgent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-mike", "mike")

	if err := reg.Register(ctx, ws, func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, factory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if factory == nil {
		t.Fatal("LookupByID() factory = nil")
	}

	agent, err := factory(ctx, ws)
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("factory() error = %v, want ErrInvalidWorkspace", err)
	}
	if agent != nil {
		t.Fatalf("factory() agent = %#v want nil", agent)
	}
}

func TestMemoryRegistry_NestedRuntimeConfigCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-november", "november")
	ws.RuntimeConfig = nestedRuntimeConfig()

	if err := reg.Register(ctx, ws, testFactory("november-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	originalMeta := ws.RuntimeConfig["meta"].(map[string]any)
	originalFlags := ws.RuntimeConfig["flags"].([]any)
	originalLabels := ws.RuntimeConfig["labels"].([]string)

	originalMeta["role"] = "mutated-after-register"
	originalFlags[0] = "mutated-after-register"
	originalLabels[0] = "mutated-after-register"

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	assertNestedConfigUnchanged(t, got.RuntimeConfig)

	gotMeta := got.RuntimeConfig["meta"].(map[string]any)
	gotFlags := got.RuntimeConfig["flags"].([]any)
	gotLabels := got.RuntimeConfig["labels"].([]string)

	gotMeta["role"] = "mutated-after-lookup"
	gotFlags[0] = "mutated-after-lookup"
	gotLabels[0] = "mutated-after-lookup"

	again, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("second LookupByID() error = %v", err)
	}
	assertNestedConfigUnchanged(t, again.RuntimeConfig)

	listed, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() len = %d want 1", len(listed))
	}
	assertNestedConfigUnchanged(t, listed[0].RuntimeConfig)
}

func TestMemoryRegistry_FactoryInputCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-oscar", "oscar")
	ws.RuntimeConfig = nestedRuntimeConfig()

	if err := reg.Register(ctx, ws, func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		ws.RuntimeConfig["meta"].(map[string]any)["role"] = "factory-mutated"
		ws.RuntimeConfig["flags"].([]any)[0] = "factory-mutated"
		ws.RuntimeConfig["labels"].([]string)[0] = "factory-mutated"
		return stubAgent{name: "oscar-agent"}, nil
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, factory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}

	agent, err := factory(ctx, ws)
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if agent == nil {
		t.Fatal("factory() agent = nil")
	}

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() after factory call error = %v", err)
	}
	assertNestedConfigUnchanged(t, got.RuntimeConfig)
}

func TestMemoryRegistry_UpdateRejectsAliasChangesAndLeavesAliasMapUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-papa", "papa")

	if err := reg.Register(ctx, ws, testFactory("papa-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	updated := ws
	updated.Alias = "quebec"

	err := reg.Update(ctx, updated)
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("Update() error = %v, want ErrInvalidWorkspace", err)
	}

	gotOriginal, _, err := reg.LookupByAlias(ctx, ws.Alias)
	if err != nil {
		t.Fatalf("LookupByAlias(original) error = %v", err)
	}
	if gotOriginal.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("LookupByAlias(original) WorkspaceID = %q want %q", gotOriginal.WorkspaceID, ws.WorkspaceID)
	}

	_, _, err = reg.LookupByAlias(ctx, updated.Alias)
	if !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("LookupByAlias(updated) error = %v, want ErrAliasNotFound", err)
	}
}

func TestMemoryRegistry_UpdatePreservesFactory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-romeo", "romeo")
	factory := testFactory("romeo-agent")

	if err := reg.Register(ctx, ws, factory); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	updated := ws
	updated.Description = "updated description"
	updated.RuntimeConfig = nestedRuntimeConfig()

	if err := reg.Update(ctx, updated); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	got, gotFactory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if got.Description != updated.Description {
		t.Fatalf("LookupByID() Description = %q want %q", got.Description, updated.Description)
	}
	if gotFactory == nil {
		t.Fatal("LookupByID() factory = nil")
	}

	agent, err := gotFactory(ctx, got)
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if agent == nil {
		t.Fatal("factory() agent = nil")
	}
	if agent.Name() != "romeo-agent" {
		t.Fatalf("factory() agent.Name() = %q want %q", agent.Name(), "romeo-agent")
	}
}

func TestMemoryRegistry_UpdatePromotesDefaultAndDemotesPrevious(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	first := testWorkspace("ws-sierra", "sierra")
	first.IsDefaultEntry = true
	second := testWorkspace("ws-tango", "tango")

	if err := reg.Register(ctx, first, testFactory("sierra-agent")); err != nil {
		t.Fatalf("Register(first) error = %v", err)
	}
	if err := reg.Register(ctx, second, testFactory("tango-agent")); err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}

	secondUpdated := second
	secondUpdated.IsDefaultEntry = true

	if err := reg.Update(ctx, secondUpdated); err != nil {
		t.Fatalf("Update(second) error = %v", err)
	}

	gotDefault, _, err := reg.Default(ctx)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if gotDefault.WorkspaceID != second.WorkspaceID {
		t.Fatalf("Default() WorkspaceID = %q want %q", gotDefault.WorkspaceID, second.WorkspaceID)
	}

	gotFirst, _, err := reg.LookupByID(ctx, first.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID(first) error = %v", err)
	}
	if gotFirst.IsDefaultEntry {
		t.Fatal("previous default remained marked after Update()")
	}
}

func TestMemoryRegistry_FailedUpdateValidationLeavesStateUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	original := testWorkspace("ws-uniform", "uniform")
	original.Description = "before"
	original.RuntimeConfig = nestedRuntimeConfig()

	if err := reg.Register(ctx, original, testFactory("uniform-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	invalid := original
	invalid.Description = strings.Repeat("x", 257)
	invalid.RuntimeConfig["meta"].(map[string]any)["role"] = "changed-before-failed-update"

	err := reg.Update(ctx, invalid)
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("Update() error = %v, want ErrInvalidWorkspace", err)
	}

	got, _, err := reg.LookupByID(ctx, original.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if got.Description != original.Description {
		t.Fatalf("LookupByID() Description = %q want %q", got.Description, original.Description)
	}
	assertNestedConfigUnchanged(t, got.RuntimeConfig)
}

func TestMemoryRegistry_TypedNestedRuntimeConfigCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-victor", "victor")
	ws.RuntimeConfig = typedRuntimeConfig()

	if err := reg.Register(ctx, ws, testFactory("victor-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	originalBlob := ws.RuntimeConfig["blob"].([]byte)
	originalCounts := ws.RuntimeConfig["counts"].(map[string]int)
	originalGroups := ws.RuntimeConfig["groups"].(map[string][]string)
	originalSteps := ws.RuntimeConfig["steps"].([]map[string]any)

	originalBlob[0] = 'z'
	originalCounts["ok"] = 99
	originalGroups["team"][0] = "mutated-after-register"
	originalSteps[0]["name"] = "mutated-after-register"

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	assertTypedConfigUnchanged(t, got.RuntimeConfig)

	gotBlob := got.RuntimeConfig["blob"].([]byte)
	gotCounts := got.RuntimeConfig["counts"].(map[string]int)
	gotGroups := got.RuntimeConfig["groups"].(map[string][]string)
	gotSteps := got.RuntimeConfig["steps"].([]map[string]any)

	gotBlob[0] = 'y'
	gotCounts["ok"] = 123
	gotGroups["team"][0] = "mutated-after-lookup"
	gotSteps[0]["name"] = "mutated-after-lookup"

	again, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("second LookupByID() error = %v", err)
	}
	assertTypedConfigUnchanged(t, again.RuntimeConfig)
}

func TestMemoryRegistry_UpdateRuntimeConfigCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-whiskey", "whiskey")

	if err := reg.Register(ctx, ws, testFactory("whiskey-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	updated := ws
	updated.RuntimeConfig = typedRuntimeConfig()

	if err := reg.Update(ctx, updated); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	updateBlob := updated.RuntimeConfig["blob"].([]byte)
	updateCounts := updated.RuntimeConfig["counts"].(map[string]int)
	updateGroups := updated.RuntimeConfig["groups"].(map[string][]string)
	updateSteps := updated.RuntimeConfig["steps"].([]map[string]any)

	updateBlob[0] = 'x'
	updateCounts["ok"] = 77
	updateGroups["team"][0] = "mutated-after-update"
	updateSteps[0]["name"] = "mutated-after-update"

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	assertTypedConfigUnchanged(t, got.RuntimeConfig)
}

func TestMemoryRegistry_AliasTrimNormalization(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-xray", "  xray  ")

	if err := reg.Register(ctx, ws, testFactory("xray-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	gotTrimmed, _, err := reg.LookupByAlias(ctx, "xray")
	if err != nil {
		t.Fatalf("LookupByAlias(trimmed) error = %v", err)
	}
	if gotTrimmed.Alias != "xray" {
		t.Fatalf("LookupByAlias(trimmed) Alias = %q want %q", gotTrimmed.Alias, "xray")
	}

	gotSpaced, _, err := reg.LookupByAlias(ctx, "  xray  ")
	if err != nil {
		t.Fatalf("LookupByAlias(spaced) error = %v", err)
	}
	if gotSpaced.Alias != "xray" {
		t.Fatalf("LookupByAlias(spaced) Alias = %q want %q", gotSpaced.Alias, "xray")
	}
}

func TestMemoryRegistry_PointerRuntimeConfigCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-bravo2", "bravo2")
	ws.RuntimeConfig = map[string]any{
		"object": newRuntimeConfigObject(),
	}

	if err := reg.Register(ctx, ws, testFactory("bravo2-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	originalObject := ws.RuntimeConfig["object"].(*runtimeConfigObject)
	originalObject.Labels[0] = "mutated-after-register"
	originalObject.Meta["role"] = "mutated-after-register"

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	assertPointerConfigObjectUnchanged(t, got.RuntimeConfig["object"])

	gotObject := got.RuntimeConfig["object"].(*runtimeConfigObject)
	gotObject.Labels[0] = "mutated-after-lookup"
	gotObject.Meta["role"] = "mutated-after-lookup"

	again, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("second LookupByID() error = %v", err)
	}
	assertPointerConfigObjectUnchanged(t, again.RuntimeConfig["object"])
}

func TestMemoryRegistry_StructRuntimeConfigCloneIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-charlie2", "charlie2")
	ws.RuntimeConfig = map[string]any{
		"object": *newRuntimeConfigObject(),
	}

	if err := reg.Register(ctx, ws, testFactory("charlie2-agent")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	originalObject := ws.RuntimeConfig["object"].(runtimeConfigObject)
	originalObject.Labels[0] = "mutated-after-register"
	originalObject.Meta["role"] = "mutated-after-register"

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	assertStructConfigObjectUnchanged(t, got.RuntimeConfig["object"])

	gotObject := got.RuntimeConfig["object"].(runtimeConfigObject)
	gotObject.Labels[0] = "mutated-after-lookup"
	gotObject.Meta["role"] = "mutated-after-lookup"

	again, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("second LookupByID() error = %v", err)
	}
	assertStructConfigObjectUnchanged(t, again.RuntimeConfig["object"])
}

func TestMemoryRegistry_FactoryInputCloneIsolationForPointerAndStructValues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reg := NewMemory()
	ws := testWorkspace("ws-delta2", "delta2")
	ws.RuntimeConfig = map[string]any{
		"pointer": newRuntimeConfigObject(),
		"struct":  *newRuntimeConfigObject(),
	}

	if err := reg.Register(ctx, ws, func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		pointerObject := ws.RuntimeConfig["pointer"].(*runtimeConfigObject)
		pointerObject.Labels[0] = "factory-mutated"
		pointerObject.Meta["role"] = "factory-mutated"

		structObject := ws.RuntimeConfig["struct"].(runtimeConfigObject)
		structObject.Labels[0] = "factory-mutated"
		structObject.Meta["role"] = "factory-mutated"

		return stubAgent{name: "delta2-agent"}, nil
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, factory, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() error = %v", err)
	}
	if factory == nil {
		t.Fatal("LookupByID() factory = nil")
	}

	agent, err := factory(ctx, ws)
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if agent == nil {
		t.Fatal("factory() agent = nil")
	}

	got, _, err := reg.LookupByID(ctx, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("LookupByID() after factory call error = %v", err)
	}
	assertPointerConfigObjectUnchanged(t, got.RuntimeConfig["pointer"])
	assertStructConfigObjectUnchanged(t, got.RuntimeConfig["struct"])
}

func TestMemoryRegistry_RegisterRejectsCyclicRuntimeConfig(t *testing.T) {
	if os.Getenv("ANA_REGISTRY_CYCLIC_REGISTER") == "1" {
		ctx := context.Background()
		reg := NewMemory()
		original := testWorkspace("ws-yankee", "yankee")
		if err := reg.Register(ctx, original, testFactory("yankee-agent")); err != nil {
			t.Fatalf("Register(original) error = %v", err)
		}

		cyclic := testWorkspace("ws-zulu", "zulu")
		cyclic.RuntimeConfig = cyclicMapRuntimeConfig()

		err := reg.Register(ctx, cyclic, testFactory("zulu-agent"))
		if !errors.Is(err, ErrInvalidWorkspace) {
			t.Fatalf("Register(cyclic) error = %v, want ErrInvalidWorkspace", err)
		}

		gotOriginal, _, err := reg.LookupByID(ctx, original.WorkspaceID)
		if err != nil {
			t.Fatalf("LookupByID(original) error = %v", err)
		}
		if !reflect.DeepEqual(gotOriginal, original) {
			t.Fatalf("LookupByID(original) = %#v want %#v", gotOriginal, original)
		}

		_, _, err = reg.LookupByID(ctx, cyclic.WorkspaceID)
		if !errors.Is(err, ErrWorkspaceNotFound) {
			t.Fatalf("LookupByID(cyclic) error = %v, want ErrWorkspaceNotFound", err)
		}
		return
	}

	runRegistrySubprocessTest(t, "ANA_REGISTRY_CYCLIC_REGISTER")
}

func TestMemoryRegistry_UpdateRejectsCyclicRuntimeConfig(t *testing.T) {
	if os.Getenv("ANA_REGISTRY_CYCLIC_UPDATE") == "1" {
		ctx := context.Background()
		reg := NewMemory()
		original := testWorkspace("ws-alpha2", "alpha2")
		original.Description = "before"
		if err := reg.Register(ctx, original, testFactory("alpha2-agent")); err != nil {
			t.Fatalf("Register(original) error = %v", err)
		}

		updated := original
		updated.Description = "after"
		updated.RuntimeConfig = cyclicSliceRuntimeConfig()

		err := reg.Update(ctx, updated)
		if !errors.Is(err, ErrInvalidWorkspace) {
			t.Fatalf("Update(cyclic) error = %v, want ErrInvalidWorkspace", err)
		}

		gotOriginal, _, err := reg.LookupByID(ctx, original.WorkspaceID)
		if err != nil {
			t.Fatalf("LookupByID(original) error = %v", err)
		}
		if gotOriginal.Description != original.Description {
			t.Fatalf("LookupByID(original) Description = %q want %q", gotOriginal.Description, original.Description)
		}
		if !reflect.DeepEqual(gotOriginal.RuntimeConfig, original.RuntimeConfig) {
			t.Fatalf("LookupByID(original) RuntimeConfig = %#v want %#v", gotOriginal.RuntimeConfig, original.RuntimeConfig)
		}
		return
	}

	runRegistrySubprocessTest(t, "ANA_REGISTRY_CYCLIC_UPDATE")
}

func testWorkspace(id, alias string) Workspace {
	return Workspace{
		WorkspaceID: id,
		Alias:       alias,
		RuntimeType: "cli",
		RuntimeKind: RuntimeKindResumableCLI,
		Description: "test workspace",
		Enabled:     true,
		RuntimeConfig: map[string]any{
			"path": "/tmp/agent",
		},
	}
}

func testFactory(name string) AgentFactory {
	return func(ctx context.Context, ws Workspace) (agentio.Agent, error) {
		return stubAgent{name: name}, nil
	}
}

type stubAgent struct {
	name string
}

func (a stubAgent) Name() string {
	return a.name
}

func (a stubAgent) Invoke(ctx context.Context, req *agentio.InvokeRequest) (agentio.EventStream, error) {
	return nil, errors.New("stub invoke not implemented")
}

func nestedRuntimeConfig() map[string]any {
	return map[string]any{
		"path": "/tmp/agent",
		"meta": map[string]any{
			"role": "worker",
		},
		"flags":  []any{"alpha", true},
		"labels": []string{"one", "two"},
	}
}

func assertNestedConfigUnchanged(t *testing.T, cfg map[string]any) {
	t.Helper()

	meta, ok := cfg["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta type = %T want map[string]any", cfg["meta"])
	}
	if got := meta["role"]; got != "worker" {
		t.Fatalf("meta.role = %v want worker", got)
	}

	flags, ok := cfg["flags"].([]any)
	if !ok {
		t.Fatalf("flags type = %T want []any", cfg["flags"])
	}
	if len(flags) != 2 {
		t.Fatalf("flags len = %d want 2", len(flags))
	}
	if flags[0] != "alpha" {
		t.Fatalf("flags[0] = %v want alpha", flags[0])
	}
	if flags[1] != true {
		t.Fatalf("flags[1] = %v want true", flags[1])
	}

	labels, ok := cfg["labels"].([]string)
	if !ok {
		t.Fatalf("labels type = %T want []string", cfg["labels"])
	}
	if len(labels) != 2 {
		t.Fatalf("labels len = %d want 2", len(labels))
	}
	if labels[0] != "one" {
		t.Fatalf("labels[0] = %q want one", labels[0])
	}
	if labels[1] != "two" {
		t.Fatalf("labels[1] = %q want two", labels[1])
	}
}

func typedRuntimeConfig() map[string]any {
	return map[string]any{
		"path": "/tmp/agent",
		"blob": []byte("abc"),
		"counts": map[string]int{
			"ok": 1,
		},
		"groups": map[string][]string{
			"team": {"red", "blue"},
		},
		"steps": []map[string]any{
			{"name": "prepare"},
			{"name": "run"},
		},
	}
}

func assertTypedConfigUnchanged(t *testing.T, cfg map[string]any) {
	t.Helper()

	blob, ok := cfg["blob"].([]byte)
	if !ok {
		t.Fatalf("blob type = %T want []byte", cfg["blob"])
	}
	if string(blob) != "abc" {
		t.Fatalf("blob = %q want %q", string(blob), "abc")
	}

	counts, ok := cfg["counts"].(map[string]int)
	if !ok {
		t.Fatalf("counts type = %T want map[string]int", cfg["counts"])
	}
	if counts["ok"] != 1 {
		t.Fatalf("counts[\"ok\"] = %d want 1", counts["ok"])
	}

	groups, ok := cfg["groups"].(map[string][]string)
	if !ok {
		t.Fatalf("groups type = %T want map[string][]string", cfg["groups"])
	}
	team := groups["team"]
	if len(team) != 2 {
		t.Fatalf("groups[\"team\"] len = %d want 2", len(team))
	}
	if team[0] != "red" {
		t.Fatalf("groups[\"team\"][0] = %q want red", team[0])
	}
	if team[1] != "blue" {
		t.Fatalf("groups[\"team\"][1] = %q want blue", team[1])
	}

	steps, ok := cfg["steps"].([]map[string]any)
	if !ok {
		t.Fatalf("steps type = %T want []map[string]any", cfg["steps"])
	}
	if len(steps) != 2 {
		t.Fatalf("steps len = %d want 2", len(steps))
	}
	if steps[0]["name"] != "prepare" {
		t.Fatalf("steps[0][\"name\"] = %v want prepare", steps[0]["name"])
	}
	if steps[1]["name"] != "run" {
		t.Fatalf("steps[1][\"name\"] = %v want run", steps[1]["name"])
	}
}

func cyclicMapRuntimeConfig() map[string]any {
	self := map[string]any{}
	self["self"] = self
	return self
}

func cyclicSliceRuntimeConfig() map[string]any {
	self := make([]any, 1)
	self[0] = self
	return map[string]any{"self": self}
}

func runRegistrySubprocessTest(t *testing.T, envKey string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), envKey+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v\n%s", err, output)
	}
}

type runtimeConfigObject struct {
	Labels []string
	Meta   map[string]string
}

func newRuntimeConfigObject() *runtimeConfigObject {
	return &runtimeConfigObject{
		Labels: []string{"one", "two"},
		Meta: map[string]string{
			"role": "worker",
		},
	}
}

func assertPointerConfigObjectUnchanged(t *testing.T, value any) {
	t.Helper()

	object, ok := value.(*runtimeConfigObject)
	if !ok {
		t.Fatalf("object type = %T want *runtimeConfigObject", value)
	}
	assertRuntimeConfigObjectUnchanged(t, *object)
}

func assertStructConfigObjectUnchanged(t *testing.T, value any) {
	t.Helper()

	object, ok := value.(runtimeConfigObject)
	if !ok {
		t.Fatalf("object type = %T want runtimeConfigObject", value)
	}
	assertRuntimeConfigObjectUnchanged(t, object)
}

func assertRuntimeConfigObjectUnchanged(t *testing.T, object runtimeConfigObject) {
	t.Helper()

	if len(object.Labels) != 2 {
		t.Fatalf("Labels len = %d want 2", len(object.Labels))
	}
	if object.Labels[0] != "one" {
		t.Fatalf("Labels[0] = %q want one", object.Labels[0])
	}
	if object.Labels[1] != "two" {
		t.Fatalf("Labels[1] = %q want two", object.Labels[1])
	}
	if object.Meta["role"] != "worker" {
		t.Fatalf("Meta[\"role\"] = %q want worker", object.Meta["role"])
	}
}
