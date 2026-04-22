package logs

import (
	"context"
	"os"
	"sync/atomic"
)

// Logger is a small structured logging facade with slog-style key/value pairs.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
	Fatal(msg string, keysAndValues ...any)
	With(keysAndValues ...any) Logger
}

type contextKey struct{}

var loggerKey contextKey

// loggerHolder gives [atomic.Value] a single concrete type (*loggerHolder) while holding any [Logger].
type loggerHolder struct {
	Logger
}

var defaultLogger atomic.Value // *loggerHolder

var exitFunc = os.Exit

// Default returns the package-level default logger.
func Default() Logger {
	return defaultLogger.Load().(*loggerHolder).Logger
}

// SetDefault replaces the package-level default logger. It panics if logger is nil.
func SetDefault(logger Logger) {
	if logger == nil {
		panic("logs: SetDefault(nil)")
	}
	defaultLogger.Store(&loggerHolder{Logger: logger})
}

// NewContext returns a new [context.Context] carrying fields merged into the default logger.
func NewContext(keysAndValues ...any) context.Context {
	return IntoContext(context.Background(), Default().With(keysAndValues...))
}

// IntoContext returns a derived context that carries logger.
func IntoContext(ctx context.Context, logger Logger) context.Context {
	if logger == nil {
		panic("logs: IntoContext(nil Logger)")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext returns the logger stored on ctx, or the default logger if none is present.
func FromContext(ctx context.Context) Logger {
	if ctx == nil {
		return Default()
	}
	v := ctx.Value(loggerKey)
	if v == nil {
		return Default()
	}
	l, ok := v.(Logger)
	if !ok || l == nil {
		return Default()
	}
	return l
}

func init() {
	defaultLogger.Store(&loggerHolder{Logger: newDefaultLogger()})
}
