package workspace

import (
	"context"
	"time"
)

const defaultRepoWriteTimeout = 5 * time.Second

func newRepoWriteContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, defaultRepoWriteTimeout)
}
