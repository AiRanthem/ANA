package claudecode

import (
	"errors"
	"testing"
)

func TestMapPath_RejectsDotDot(t *testing.T) {
	t.Parallel()

	_, err := mapPath(".claude/plugins/demo", "..")
	if err == nil {
		t.Fatal("mapPath: expected error for .. segment")
	}
	if !errors.Is(err, ErrInvalidPluginLayout) {
		t.Fatalf("mapPath error = %v, want ErrInvalidPluginLayout", err)
	}
}
