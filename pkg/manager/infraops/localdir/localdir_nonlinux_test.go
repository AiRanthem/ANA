//go:build !linux

package localdir

import (
	"context"
	"errors"
	"testing"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

func TestExec_FailsClosedOnNonLinux(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)
	_, err := ops.Exec(context.Background(), infraops.ExecCommand{Program: "pwd"})
	if !errors.Is(err, infraops.ErrPathOutsideDir) {
		t.Fatalf("Exec() error = %v, want ErrPathOutsideDir", err)
	}
}
