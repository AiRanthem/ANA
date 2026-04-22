package logs

import (
	"context"
	"log/slog"
	"os"
)

// LevelFatal is a custom slog level above [slog.LevelError].
const LevelFatal = slog.Level(12)

type slogLogger struct {
	log *slog.Logger
}

// NewSlog wraps an existing [*slog.Logger]. It panics if logger is nil.
func NewSlog(logger *slog.Logger) Logger {
	if logger == nil {
		panic("logs: NewSlog(nil)")
	}
	return &slogLogger{log: logger}
}

func (l *slogLogger) Debug(msg string, keysAndValues ...any) {
	l.log.Debug(msg, keysAndValues...)
}

func (l *slogLogger) Info(msg string, keysAndValues ...any) {
	l.log.Info(msg, keysAndValues...)
}

func (l *slogLogger) Warn(msg string, keysAndValues ...any) {
	l.log.Warn(msg, keysAndValues...)
}

func (l *slogLogger) Error(msg string, keysAndValues ...any) {
	l.log.Error(msg, keysAndValues...)
}

func (l *slogLogger) Fatal(msg string, keysAndValues ...any) {
	l.log.Log(context.Background(), LevelFatal, msg, keysAndValues...)
	exitFunc(1)
}

func (l *slogLogger) With(keysAndValues ...any) Logger {
	return &slogLogger{log: l.log.With(keysAndValues...)}
}

func newDefaultLogger() Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: false,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelFatal {
					return slog.Attr{Key: slog.LevelKey, Value: slog.StringValue("FATAL")}
				}
			}
			return a
		},
	})
	return &slogLogger{log: slog.New(h)}
}

func init() {
	defaultLogger.Store(newDefaultLogger())
}
