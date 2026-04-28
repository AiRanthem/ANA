package workspace

import (
	"context"
	"time"
)

const defaultRepoWriteTimeout = 5 * time.Second

func newRepoWriteContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, defaultRepoWriteTimeout)
}
