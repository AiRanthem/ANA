package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryRepository_InsertAliasConflictAndListOrdering(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	now := time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC)

	rows := []Workspace{
		testWorkspace("wsp_3", "zeta", "charlie", StatusInit, now.Add(2*time.Minute)),
		testWorkspace("wsp_1", "alpha", "beta", StatusHealthy, now),
		testWorkspace("wsp_4", "alpha", "alpha", StatusFailed, now.Add(3*time.Minute)),
		testWorkspace("wsp_2", "alpha", "zulu", StatusHealthy, now.Add(time.Minute)),
		testWorkspace("wsp_5", "beta", "alpha", StatusHealthy, now.Add(4*time.Minute)),
	}

	for _, row := range rows {
		if err := repo.Insert(context.Background(), row); err != nil {
			t.Fatalf("Insert(%q) error = %v", row.ID, err)
		}
	}

	conflict := testWorkspace("wsp_conflict", "alpha", "alpha", StatusInit, now.Add(5*time.Minute))
	if err := repo.Insert(context.Background(), conflict); !errors.Is(err, ErrAliasConflict) {
		t.Fatalf("duplicate alias Insert() error = %v, want ErrAliasConflict", err)
	}

	otherNamespace := testWorkspace("wsp_other_ns", "omega", "alpha", StatusInit, now.Add(6*time.Minute))
	if err := repo.Insert(context.Background(), otherNamespace); err != nil {
		t.Fatalf("cross-namespace alias Insert() error = %v", err)
	}

	page1, next, err := repo.List(context.Background(), ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("List(page1) error = %v", err)
	}
	if got, want := workspaceAliases(page1), []string{"alpha/alpha", "alpha/beta", "alpha/zulu"}; !equalStrings(got, want) {
		t.Fatalf("List(page1) aliases = %v, want %v", got, want)
	}
	if next == "" {
		t.Fatalf("List(page1) next cursor is empty")
	}

	page2, next, err := repo.List(context.Background(), ListOptions{Limit: 3, Cursor: next})
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

func TestMemoryRepository_UpdateStatusTransitions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		initial Status
		next    Status
		wantErr error
	}{
		{name: "init to init", initial: StatusInit, next: StatusInit},
		{name: "init to healthy", initial: StatusInit, next: StatusHealthy},
		{name: "init to failed", initial: StatusInit, next: StatusFailed},
		{name: "healthy to failed", initial: StatusHealthy, next: StatusFailed},
		{name: "failed to healthy", initial: StatusFailed, next: StatusHealthy},
		{name: "healthy to init", initial: StatusHealthy, next: StatusInit, wantErr: ErrInvalidStatusTransition},
		{name: "failed to init", initial: StatusFailed, next: StatusInit, wantErr: ErrInvalidStatusTransition},
		{name: "healthy to healthy", initial: StatusHealthy, next: StatusHealthy, wantErr: ErrInvalidStatusTransition},
		{name: "failed to failed", initial: StatusFailed, next: StatusFailed, wantErr: ErrInvalidStatusTransition},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := NewMemoryRepository()
			row := testWorkspace("wsp_status", "default", "alpha", tt.initial, now)
			if err := repo.Insert(context.Background(), row); err != nil {
				t.Fatalf("Insert() error = %v", err)
			}

			statusErr := &Error{
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
			if tt.next == StatusHealthy && got.StatusError != nil {
				t.Fatalf("Get().StatusError = %#v, want nil on healthy", got.StatusError)
			}
			if tt.next == StatusFailed {
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

func TestMemoryRepository_UpdateStatusCAS_RejectsWrongExpectedStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	row := testWorkspace("wsp_cas_expect", "default", "alpha", StatusHealthy, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	err := repo.UpdateStatusCAS(context.Background(), row.ID, StatusWriterScheduler, StatusInit, StatusFailed, nil, time.Time{})
	if !errors.Is(err, ErrStatusPreconditionFailed) {
		t.Fatalf("UpdateStatusCAS() error = %v, want ErrStatusPreconditionFailed", err)
	}
}

func TestMemoryRepository_UpdateStatusCAS_RejectsWriterForbiddenTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	row := testWorkspace("wsp_cas_writer", "default", "alpha", StatusInit, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	err := repo.UpdateStatusCAS(context.Background(), row.ID, StatusWriterScheduler, StatusInit, StatusHealthy, nil, time.Time{})
	if !errors.Is(err, ErrInvalidStatusTransition) {
		t.Fatalf("UpdateStatusCAS() error = %v, want ErrInvalidStatusTransition", err)
	}
}

func TestMemoryRepository_UpdateStatusCAS_AllowsControllerInitToHealthy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	row := testWorkspace("wsp_cas_ctrl_ok", "default", "alpha", StatusInit, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	probedAt := now.Add(time.Second)
	if err := repo.UpdateStatusCAS(context.Background(), row.ID, StatusWriterController, StatusInit, StatusHealthy, nil, probedAt); err != nil {
		t.Fatalf("UpdateStatusCAS() error = %v", err)
	}
	got, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusHealthy {
		t.Fatalf("Status = %q, want healthy", got.Status)
	}
	if !got.LastProbeAt.Equal(probedAt) {
		t.Fatalf("LastProbeAt = %v, want %v", got.LastProbeAt, probedAt)
	}
}

func TestMemoryRepository_UpdateStatusCAS_AllowsSchedulerFailedToHealthy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	row := testWorkspace("wsp_cas_sched_ok", "default", "alpha", StatusFailed, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	probedAt := now.Add(2 * time.Second)
	if err := repo.UpdateStatusCAS(context.Background(), row.ID, StatusWriterScheduler, StatusFailed, StatusHealthy, nil, probedAt); err != nil {
		t.Fatalf("UpdateStatusCAS() error = %v", err)
	}
	got, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusHealthy {
		t.Fatalf("Status = %q, want healthy", got.Status)
	}
	if got.StatusError != nil {
		t.Fatalf("StatusError = %#v, want nil", got.StatusError)
	}
}

func TestMemoryRepository_UpdateRejectsStatusMutation(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	row := testWorkspace("wsp_update", "default", "alpha", StatusInit, time.Date(2026, time.April, 26, 10, 0, 0, 0, time.UTC))
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	row.Status = StatusHealthy
	if err := repo.Update(context.Background(), row); !errors.Is(err, ErrInvalidStatusTransition) {
		t.Fatalf("Update() error = %v, want ErrInvalidStatusTransition", err)
	}
}

func testWorkspace(id WorkspaceID, namespace Namespace, alias Alias, status Status, now time.Time) Workspace {
	return Workspace{
		ID:            id,
		Namespace:     namespace,
		Alias:         alias,
		AgentType:     AgentType("claude-code"),
		InfraType:     InfraType("localdir"),
		InfraOptions:  map[string]any{"dir": string(id)},
		InstallParams: map[string]any{"token": "secret"},
		Plugins: []AttachedPlugin{
			{
				PluginID:    PluginID("plg_a"),
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

func workspaceAliases(rows []Workspace) []string {
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
