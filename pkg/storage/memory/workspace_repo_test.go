package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

func TestWorkspaceRepository_InsertAliasConflictAndListOrdering(t *testing.T) {
	t.Parallel()

	repo := NewWorkspaceRepository()
	now := time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC)

	rows := []workspace.Workspace{
		testWorkspace("wsp_3", "zeta", "charlie", workspace.StatusInit, now.Add(2*time.Minute)),
		testWorkspace("wsp_1", "alpha", "beta", workspace.StatusHealthy, now),
		testWorkspace("wsp_4", "alpha", "alpha", workspace.StatusFailed, now.Add(3*time.Minute)),
		testWorkspace("wsp_2", "alpha", "zulu", workspace.StatusHealthy, now.Add(time.Minute)),
		testWorkspace("wsp_5", "beta", "alpha", workspace.StatusHealthy, now.Add(4*time.Minute)),
	}

	for _, row := range rows {
		if err := repo.Insert(context.Background(), row); err != nil {
			t.Fatalf("Insert(%q) error = %v", row.ID, err)
		}
	}

	conflict := testWorkspace("wsp_conflict", "alpha", "alpha", workspace.StatusInit, now.Add(5*time.Minute))
	if err := repo.Insert(context.Background(), conflict); !errors.Is(err, workspace.ErrAliasConflict) {
		t.Fatalf("duplicate alias Insert() error = %v, want ErrAliasConflict", err)
	}

	otherNamespace := testWorkspace("wsp_other_ns", "omega", "alpha", workspace.StatusInit, now.Add(6*time.Minute))
	if err := repo.Insert(context.Background(), otherNamespace); err != nil {
		t.Fatalf("cross-namespace alias Insert() error = %v", err)
	}

	page1, next, err := repo.List(context.Background(), workspace.ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("List(page1) error = %v", err)
	}
	if got, want := workspaceAliases(page1), []string{"alpha/alpha", "alpha/beta", "alpha/zulu"}; !equalStrings(got, want) {
		t.Fatalf("List(page1) aliases = %v, want %v", got, want)
	}
	if next == "" {
		t.Fatalf("List(page1) next cursor is empty")
	}

	page2, next, err := repo.List(context.Background(), workspace.ListOptions{Limit: 3, Cursor: next})
	if err != nil {
		t.Fatalf("List(page2) error = %v", err)
	}
	if got, want := workspaceAliases(page2), []string{"beta/alpha", "omega/alpha", "zeta/charlie"}; !equalStrings(got, want) {
		t.Fatalf("List(page2) aliases = %v, want %v", got, want)
	}
	if next != "" {
		t.Fatalf("List(page2) next = %q, want empty", next)
	}
}

func TestWorkspaceRepository_AliasIndexUsesStructuredKey(t *testing.T) {
	t.Parallel()

	repo := NewWorkspaceRepository()
	now := time.Now().UTC()
	first := testWorkspace("wsp_first", "a", "b\x00c", workspace.StatusInit, now)
	second := testWorkspace("wsp_second", "a\x00b", "c", workspace.StatusInit, now.Add(time.Second))

	if err := repo.Insert(context.Background(), first); err != nil {
		t.Fatalf("Insert(first) error = %v", err)
	}
	if err := repo.Insert(context.Background(), second); err != nil {
		t.Fatalf("Insert(second) error = %v", err)
	}

	gotFirst, err := repo.GetByAlias(context.Background(), first.Namespace, first.Alias)
	if err != nil {
		t.Fatalf("GetByAlias(first) error = %v", err)
	}
	if gotFirst.ID != first.ID {
		t.Fatalf("GetByAlias(first).ID = %q, want %q", gotFirst.ID, first.ID)
	}

	gotSecond, err := repo.GetByAlias(context.Background(), second.Namespace, second.Alias)
	if err != nil {
		t.Fatalf("GetByAlias(second) error = %v", err)
	}
	if gotSecond.ID != second.ID {
		t.Fatalf("GetByAlias(second).ID = %q, want %q", gotSecond.ID, second.ID)
	}
}

func TestWorkspaceRepository_UpdateStatusTransitions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		initial workspace.Status
		next    workspace.Status
		wantErr error
	}{
		{name: "init to init", initial: workspace.StatusInit, next: workspace.StatusInit},
		{name: "init to healthy", initial: workspace.StatusInit, next: workspace.StatusHealthy},
		{name: "init to failed", initial: workspace.StatusInit, next: workspace.StatusFailed},
		{name: "healthy to failed", initial: workspace.StatusHealthy, next: workspace.StatusFailed},
		{name: "failed to healthy", initial: workspace.StatusFailed, next: workspace.StatusHealthy},
		{name: "healthy to init", initial: workspace.StatusHealthy, next: workspace.StatusInit, wantErr: workspace.ErrInvalidStatusTransition},
		{name: "failed to init", initial: workspace.StatusFailed, next: workspace.StatusInit, wantErr: workspace.ErrInvalidStatusTransition},
		{name: "healthy to healthy", initial: workspace.StatusHealthy, next: workspace.StatusHealthy, wantErr: workspace.ErrInvalidStatusTransition},
		{name: "failed to failed", initial: workspace.StatusFailed, next: workspace.StatusFailed, wantErr: workspace.ErrInvalidStatusTransition},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := NewWorkspaceRepository()
			row := testWorkspace("wsp_status", "default", "alpha", tt.initial, now)
			if err := repo.Insert(context.Background(), row); err != nil {
				t.Fatalf("Insert() error = %v", err)
			}

			statusErr := &workspace.Error{
				Code:       "test.failure",
				Message:    "boom",
				Phase:      "test",
				RecordedAt: now.Add(time.Second),
			}
			err := repo.UpdateStatus(context.Background(), row.ID, tt.next, statusErr, now.Add(2*time.Second))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("UpdateStatus() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}

			got, err := repo.Get(context.Background(), row.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != tt.next {
				t.Fatalf("Get().Status = %q, want %q", got.Status, tt.next)
			}
			if tt.next == workspace.StatusHealthy && got.StatusError != nil {
				t.Fatalf("Get().StatusError = %#v, want nil on healthy", got.StatusError)
			}
			if tt.next == workspace.StatusFailed {
				if got.StatusError == nil {
					t.Fatalf("Get().StatusError = nil, want non-nil")
				}
				if got.StatusError.Code != statusErr.Code {
					t.Fatalf("Get().StatusError.Code = %q, want %q", got.StatusError.Code, statusErr.Code)
				}
			}
		})
	}
}

func TestWorkspaceRepository_UpdateStatusCAS_RejectsWrongExpectedStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewWorkspaceRepository()
	row := testWorkspace("wsp_cas_expect", "default", "alpha", workspace.StatusHealthy, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	err := repo.UpdateStatusCAS(context.Background(), row.ID, workspace.StatusWriterScheduler, workspace.StatusInit, workspace.StatusFailed, nil, time.Time{})
	if !errors.Is(err, workspace.ErrStatusPreconditionFailed) {
		t.Fatalf("UpdateStatusCAS() error = %v, want ErrStatusPreconditionFailed", err)
	}
}

func TestWorkspaceRepository_UpdateStatusCAS_RejectsWriterForbiddenTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewWorkspaceRepository()
	row := testWorkspace("wsp_cas_writer", "default", "alpha", workspace.StatusInit, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	err := repo.UpdateStatusCAS(context.Background(), row.ID, workspace.StatusWriterScheduler, workspace.StatusInit, workspace.StatusHealthy, nil, time.Time{})
	if !errors.Is(err, workspace.ErrInvalidStatusTransition) {
		t.Fatalf("UpdateStatusCAS() error = %v, want ErrInvalidStatusTransition", err)
	}
}

func TestWorkspaceRepository_UpdateStatusCAS_AllowsControllerInitToHealthy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewWorkspaceRepository()
	row := testWorkspace("wsp_cas_ctrl_ok", "default", "alpha", workspace.StatusInit, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	probedAt := now.Add(time.Second)
	if err := repo.UpdateStatusCAS(context.Background(), row.ID, workspace.StatusWriterController, workspace.StatusInit, workspace.StatusHealthy, nil, probedAt); err != nil {
		t.Fatalf("UpdateStatusCAS() error = %v", err)
	}
	got, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != workspace.StatusHealthy {
		t.Fatalf("Status = %q, want healthy", got.Status)
	}
	if !got.LastProbeAt.Equal(probedAt) {
		t.Fatalf("LastProbeAt = %v, want %v", got.LastProbeAt, probedAt)
	}
}

func TestWorkspaceRepository_UpdateStatusCAS_AllowsSchedulerFailedToHealthy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewWorkspaceRepository()
	row := testWorkspace("wsp_cas_sched_ok", "default", "alpha", workspace.StatusFailed, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	probedAt := now.Add(2 * time.Second)
	if err := repo.UpdateStatusCAS(context.Background(), row.ID, workspace.StatusWriterScheduler, workspace.StatusFailed, workspace.StatusHealthy, nil, probedAt); err != nil {
		t.Fatalf("UpdateStatusCAS() error = %v", err)
	}
	got, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != workspace.StatusHealthy {
		t.Fatalf("Status = %q, want healthy", got.Status)
	}
	if got.StatusError != nil {
		t.Fatalf("StatusError = %#v, want nil", got.StatusError)
	}
}

func TestWorkspaceRepository_UpdateRejectsStatusMutation(t *testing.T) {
	t.Parallel()

	repo := NewWorkspaceRepository()
	row := testWorkspace("wsp_update", "default", "alpha", workspace.StatusInit, time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC))
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	row.Status = workspace.StatusHealthy
	if err := repo.Update(context.Background(), row); !errors.Is(err, workspace.ErrInvalidStatusTransition) {
		t.Fatalf("Update() error = %v, want ErrInvalidStatusTransition", err)
	}
}

func testWorkspace(id workspace.WorkspaceID, namespace workspace.Namespace, alias workspace.Alias, status workspace.Status, now time.Time) workspace.Workspace {
	return workspace.Workspace{
		ID:            id,
		Namespace:     namespace,
		Alias:         alias,
		AgentType:     workspace.AgentType("claude-code"),
		InfraType:     workspace.InfraType("localdir"),
		InfraOptions:  map[string]any{"dir": string(id)},
		InstallParams: map[string]any{"token": "secret"},
		Plugins: []workspace.AttachedPlugin{
			{
				PluginID:    workspace.PluginID("plg_a"),
				Name:        "plugin-a",
				ContentHash: "sha256:aaa",
			},
		},
		Status:      status,
		Description: "desc",
		Labels: map[string]string{
			"env": "test",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func workspaceAliases(rows []workspace.Workspace) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, string(row.Namespace)+"/"+string(row.Alias))
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
