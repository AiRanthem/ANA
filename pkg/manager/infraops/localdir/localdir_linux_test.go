//go:build linux

package localdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

func TestExec_RejectsWorkspaceRootReplacedBySymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	other := filepath.Join(root, "other-target")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}

	ops := mustNewOps(t, infraops.Options{"dir": dir})
	ctx := context.Background()
	if err := ops.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := ops.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}

	moved := filepath.Join(root, "moved-away")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ops.Exec(ctx, infraops.ExecCommand{Program: "pwd"})
	if !errors.Is(err, infraops.ErrPathOutsideDir) {
		t.Fatalf("Exec() error = %v, want ErrPathOutsideDir", err)
	}
}
