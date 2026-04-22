package logs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestFromContext_NilContext(t *testing.T) {
	//lint:ignore SA1012 intentional nil ctx for FromContext(nil) fallback behavior
	l := FromContext(nil)
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
	l.Info("ok")
}

func TestFromContext_BackgroundWithoutValue(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	SetDefault(NewSlog(slog.New(h)))

	l := FromContext(context.Background())
	l.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("expected log output, got %q", buf.String())
	}
}

func TestNewContext_CarriesFields(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	SetDefault(NewSlog(slog.New(h)))

	ctx := NewContext("request_id", "r-1")
	FromContext(ctx).Info("start")
	out := buf.String()
	if !strings.Contains(out, "request_id") || !strings.Contains(out, "r-1") {
		t.Fatalf("expected request_id in output, got %q", out)
	}
}

func TestWith_AppendsFields(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := NewSlog(slog.New(h))
	child := base.With("component", "worker")

	child.Info("tick", "n", 1)
	out := buf.String()
	for _, want := range []string{"component", "worker", "tick", "n", "1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output %q", want, out)
		}
	}
}

type ctxMarker struct{}

func TestIntoContext_PreservesOtherValues(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlog(slog.New(h))

	ctx := context.WithValue(context.Background(), ctxMarker{}, "kept")
	ctx2 := IntoContext(ctx, logger)
	if got := ctx2.Value(ctxMarker{}); got != "kept" {
		t.Fatalf("expected marker preserved, got %v", got)
	}
	FromContext(ctx2).Info("injected")
	if !strings.Contains(buf.String(), "injected") {
		t.Fatalf("expected injected log, got %q", buf.String())
	}
}

func TestIntoContext_NilContextUsesBackground(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlog(slog.New(h))

	//lint:ignore SA1012 intentional nil ctx for IntoContext(nil, ...) → Background branch
	ctx := IntoContext(nil, logger)
	FromContext(ctx).Info("root")
	if !strings.Contains(buf.String(), "root") {
		t.Fatalf("expected log, got %q", buf.String())
	}
}

func TestSetDefault_AffectsNewContextAndFromContextFallback(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	var bufA bytes.Buffer
	loggerA := NewSlog(slog.New(slog.NewTextHandler(&bufA, &slog.HandlerOptions{Level: slog.LevelDebug})))
	SetDefault(loggerA)

	ctx := NewContext("a", "1")
	FromContext(ctx).Info("from-new-context")
	if !strings.Contains(bufA.String(), "from-new-context") {
		t.Fatalf("expected bufA to receive logs, got %q", bufA.String())
	}

	var bufB bytes.Buffer
	loggerB := NewSlog(slog.New(slog.NewTextHandler(&bufB, &slog.HandlerOptions{Level: slog.LevelDebug})))
	SetDefault(loggerB)

	// ctx still carries the logger derived from loggerA.
	FromContext(ctx).Info("after-switch")
	if !strings.Contains(bufA.String(), "after-switch") {
		t.Fatalf("expected ctx-bound logger to keep using bufA, got bufA=%q bufB=%q", bufA.String(), bufB.String())
	}
	// Fallback path: fresh background should use loggerB
	FromContext(context.Background()).Info("fallback")
	if !strings.Contains(bufB.String(), "fallback") {
		t.Fatalf("expected fallback to bufB, got %q", bufB.String())
	}

	// NewContext after switch should use loggerB as base.
	ctx2 := NewContext("b", "2")
	FromContext(ctx2).Info("newctx-after-switch")
	if !strings.Contains(bufB.String(), "newctx-after-switch") {
		t.Fatalf("expected NewContext to use new default, got %q", bufB.String())
	}
}

func TestFatal_LogsThenExitsWithCode1(t *testing.T) {
	prev := Default()
	prevExit := exitFunc
	t.Cleanup(func() {
		SetDefault(prev)
		exitFunc = prevExit
	})

	var buf bytes.Buffer
	ho := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelFatal {
					return slog.Attr{Key: slog.LevelKey, Value: slog.StringValue("FATAL")}
				}
			}
			return a
		},
	}
	SetDefault(NewSlog(slog.New(slog.NewTextHandler(&buf, ho))))

	var got int
	exitFunc = func(code int) { got = code }

	Default().Fatal("stop", "reason", "test")
	if got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}
	out := buf.String()
	if !strings.Contains(out, "FATAL") || !strings.Contains(out, "stop") {
		t.Fatalf("expected FATAL log, got %q", out)
	}
}

func TestFatal_ExitsEvenWhenFiltered(t *testing.T) {
	prev := Default()
	prevExit := exitFunc
	t.Cleanup(func() {
		SetDefault(prev)
		exitFunc = prevExit
	})

	// Discard everything, including fatal records.
	SetDefault(NewSlog(slog.New(slog.DiscardHandler)))

	var got int
	exitFunc = func(code int) { got = code }

	Default().Fatal("invisible")
	if got != 1 {
		t.Fatalf("expected exit code 1 even when logs are discarded, got %d", got)
	}
}

func TestNewSlog_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = NewSlog(nil)
}

func TestSetDefault_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	SetDefault(nil)
}

// fakeLogger is a minimal [Logger] implementation distinct from *slogLogger.
// Regression: [SetDefault] must accept any valid [Logger] without atomic.Value panicking.
type fakeLogger struct {
	debug, info bool
}

func (f *fakeLogger) Debug(string, ...any) { f.debug = true }
func (f *fakeLogger) Info(string, ...any)  { f.info = true }
func (f *fakeLogger) Warn(string, ...any)  {}
func (f *fakeLogger) Error(string, ...any) {}
func (f *fakeLogger) Fatal(string, ...any) {}
func (f *fakeLogger) With(...any) Logger   { return f }

func TestSetDefault_AcceptsNonSlogLogger(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { SetDefault(prev) })

	f := &fakeLogger{}
	SetDefault(f)

	Default().Info("x")
	if !f.info {
		t.Fatal("expected custom logger to receive Info")
	}
}

func TestIntoContext_NilLoggerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = IntoContext(context.Background(), nil)
}
