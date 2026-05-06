package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewRepoWriteContext_CancelsWithParent(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := newRepoWriteContext(parent)
	defer cancel()

	cancelParent()

	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("repo write context did not cancel with parent")
	}

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}
