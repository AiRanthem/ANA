package localdir

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	validDir := filepath.Join(t.TempDir(), "workspace")

	tests := []struct {
		name      string
		opts      infraops.Options
		wantIs    []error
		wantNotIs []error
	}{
		{
			name:   "missing dir",
			opts:   infraops.Options{},
			wantIs: []error{ErrInvalidDir, infraops.ErrInvalidOption},
		},
		{
			name:   "relative dir",
			opts:   infraops.Options{"dir": "relative/path"},
			wantIs: []error{ErrInvalidDir, infraops.ErrInvalidOption},
		},
		{
			name:   "denied dir",
			opts:   infraops.Options{"dir": "/"},
			wantIs: []error{ErrInvalidDir, infraops.ErrInvalidOption},
		},
		{
			name:      "unknown option",
			opts:      infraops.Options{"dir": validDir, "unknown": true},
			wantIs:    []error{infraops.ErrInvalidOption},
			wantNotIs: []error{ErrInvalidDir},
		},
		{
			name:   "invalid keep_dir type",
			opts:   infraops.Options{"dir": validDir, "keep_dir": "true"},
			wantIs: []error{infraops.ErrInvalidOption},
		},
		{
			name:   "invalid umask type",
			opts:   infraops.Options{"dir": validDir, "umask": "022"},
			wantIs: []error{infraops.ErrInvalidOption},
		},
		{
			name:   "invalid base_env type",
			opts:   infraops.Options{"dir": validDir, "base_env": "LANG=C"},
			wantIs: []error{infraops.ErrInvalidOption},
		},
		{
			name:   "invalid request_loopback_host",
			opts:   infraops.Options{"dir": validDir, "request_loopback_host": ""},
			wantIs: []error{infraops.ErrInvalidOption},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(context.Background(), tt.opts)
			if err == nil {
				t.Fatalf("expected error")
			}
			for _, want := range tt.wantIs {
				if !errors.Is(err, want) {
					t.Fatalf("expected error %v, got %v", want, err)
				}
			}
			for _, wantNot := range tt.wantNotIs {
				if errors.Is(err, wantNot) {
					t.Fatalf("did not expect error %v, got %v", wantNot, err)
				}
			}
		})
	}
}

func TestInitEmptyAndNonEmptyDir(t *testing.T) {
	t.Parallel()

	t.Run("creates missing dir", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		dir := filepath.Join(root, "workspace")
		ops := mustNewOps(t, infraops.Options{"dir": dir})

		if err := ops.Init(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("expected directory")
		}
	})

	t.Run("accepts empty existing dir", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		ops := mustNewOps(t, infraops.Options{"dir": dir})
		if err := ops.Init(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
	})

	t.Run("rejects non empty existing dir", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
			t.Fatalf("write seed: %v", err)
		}

		ops := mustNewOps(t, infraops.Options{"dir": dir})
		err := ops.Init(context.Background())
		if !errors.Is(err, infraops.ErrAlreadyInitialized) {
			t.Fatalf("expected ErrAlreadyInitialized, got %v", err)
		}
		if !errors.Is(err, ErrDirNotEmpty) {
			t.Fatalf("expected ErrDirNotEmpty, got %v", err)
		}

		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read seed: %v", readErr)
		}
		if string(got) != "keep" {
			t.Fatalf("seed changed unexpectedly: %q", string(got))
		}
	})
}

func TestExecRejectsEscapingWorkDir(t *testing.T) {
	t.Parallel()
	requireProgram(t, "true")

	ops := initOpsInTempDir(t, nil)
	_, err := ops.Exec(context.Background(), infraops.ExecCommand{
		Program: "true",
		WorkDir: "../escape",
	})
	if !errors.Is(err, infraops.ErrPathOutsideDir) {
		t.Fatalf("expected ErrPathOutsideDir, got %v", err)
	}
}

func TestExecNonZeroExitReturnsResult(t *testing.T) {
	t.Parallel()
	requireProgram(t, "false")

	ops := initOpsInTempDir(t, nil)
	result, err := ops.Exec(context.Background(), infraops.ExecCommand{
		Program: "false",
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero", result.ExitCode)
	}
}

func TestExecHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	requireProgram(t, "sleep")

	ops := initOpsInTempDir(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ops.Exec(ctx, infraops.ExecCommand{
		Program: "sleep",
		Args:    []string{"5"},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("exec cancellation took too long: %s", elapsed)
	}
}

func TestPutFileIsAtomicForReaders(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "workspace")
	ops := mustNewOps(t, infraops.Options{"dir": dir})
	if err := ops.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	oldData := bytes.Repeat([]byte("A"), 256*1024)
	newData := bytes.Repeat([]byte("B"), 1024*1024)
	fullPath := filepath.Join(dir, "state.bin")
	if err := os.WriteFile(fullPath, oldData, 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	stop := make(chan struct{})
	var sawPartial atomic.Bool
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(fullPath)
			if err != nil {
				continue
			}
			if !bytes.Equal(got, oldData) && !bytes.Equal(got, newData) {
				sawPartial.Store(true)
				return
			}
		}
	}()

	err := ops.PutFile(context.Background(), "state.bin", &slowReader{
		data:  newData,
		chunk: 1024,
		sleep: 250 * time.Microsecond,
	}, 0o644)
	close(stop)
	readerWG.Wait()
	if err != nil {
		t.Fatalf("put file: %v", err)
	}
	if sawPartial.Load() {
		t.Fatalf("reader observed partial write")
	}

	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatalf("final content mismatch")
	}
}

func TestClearWaitsForInFlightPutFile(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)

	reader := &gatedReader{
		data:    bytes.Repeat([]byte("z"), 128*1024),
		chunk:   1024,
		started: make(chan struct{}),
		allow:   make(chan struct{}),
	}

	putErrCh := make(chan error, 1)
	go func() {
		putErrCh <- ops.PutFile(context.Background(), "inflight.bin", reader, 0o644)
	}()

	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PutFile to start")
	}

	clearCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clearErrCh := make(chan error, 1)
	go func() {
		clearErrCh <- ops.Clear(clearCtx)
	}()

	time.Sleep(50 * time.Millisecond)
	close(reader.allow)

	select {
	case err := <-putErrCh:
		if err != nil {
			t.Fatalf("PutFile failed while Clear in flight: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for PutFile result")
	}

	select {
	case err := <-clearErrCh:
		if err != nil {
			t.Fatalf("Clear failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Clear result")
	}

	if err := ops.PutFile(context.Background(), "after-clear.txt", strings.NewReader("x"), 0o644); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("expected ErrCleared after clear, got %v", err)
	}
}

func TestPutFileNilContentReturnsInvalidOption(t *testing.T) {
	t.Parallel()
	ops := initOpsInTempDir(t, nil)

	err := ops.PutFile(context.Background(), "x.txt", nil, 0o644)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, infraops.ErrInvalidOption) {
		t.Fatalf("expected ErrInvalidOption, got %v", err)
	}
}

func TestClearReturnsContextErrorWhileWaitingForInflightOp(t *testing.T) {
	t.Parallel()
	ops := initOpsInTempDir(t, nil)

	reader := &gatedReader{
		data:    bytes.Repeat([]byte("z"), 128*1024),
		chunk:   1024,
		started: make(chan struct{}),
		allow:   make(chan struct{}),
	}
	putErrCh := make(chan error, 1)
	go func() {
		putErrCh <- ops.PutFile(context.Background(), "inflight.bin", reader, 0o644)
	}()

	<-reader.started

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	clearErrCh := make(chan error, 1)
	go func() { clearErrCh <- ops.Clear(ctx) }()

	err := <-clearErrCh
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}

	close(reader.allow)
	_ = <-putErrCh
}

func TestInitAndOpenReturnContextErrorWhileClearing(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)
	reader := &gatedReader{
		data:    bytes.Repeat([]byte("a"), 64*1024),
		chunk:   1024,
		started: make(chan struct{}),
		allow:   make(chan struct{}),
	}
	putErrCh := make(chan error, 1)
	go func() {
		putErrCh <- ops.PutFile(context.Background(), "hold.bin", reader, 0o644)
	}()
	<-reader.started

	clearErrCh := make(chan error, 1)
	go func() { clearErrCh <- ops.Clear(context.Background()) }()

	// Let Clear set clearing=true and block on activeOps before Init/Open run;
	// otherwise they can observe clearing==false and return early (e.g. ErrAlreadyInitialized).
	time.Sleep(50 * time.Millisecond)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel1()
	if err := ops.Init(ctx1); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Init expected context error, got %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if err := ops.Open(ctx2); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Open expected context error, got %v", err)
	}

	close(reader.allow)
	_ = <-putErrCh
	_ = <-clearErrCh
}

func TestOperationBeginOpReturnsContextErrorWhileClearing(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)
	reader := &gatedReader{
		data:    bytes.Repeat([]byte("b"), 64*1024),
		chunk:   1024,
		started: make(chan struct{}),
		allow:   make(chan struct{}),
	}
	putErrCh := make(chan error, 1)
	go func() {
		putErrCh <- ops.PutFile(context.Background(), "hold2.bin", reader, 0o644)
	}()
	<-reader.started

	clearErrCh := make(chan error, 1)
	go func() { clearErrCh <- ops.Clear(context.Background()) }()

	// Same ordering as TestInitAndOpenReturnContextErrorWhileClearing: ensure Clear holds clearing.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := ops.PutFile(ctx, "during-clear.txt", strings.NewReader("y"), 0o644)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("PutFile expected context error, got %v", err)
	}

	close(reader.allow)
	_ = <-putErrCh
	_ = <-clearErrCh
}

func TestGetFileReadHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)
	if err := ops.PutFile(context.Background(), "data.txt", strings.NewReader("hello"), 0o644); err != nil {
		t.Fatalf("put file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := ops.GetFile(ctx, "data.txt")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	cancel()
	buf := make([]byte, 8)
	_, err = rc.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRequestOverridesHost(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok:"+r.URL.Path)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: mux}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	go func() {
		_ = srv.Serve(ln)
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	ops := initOpsInTempDir(t, infraops.Options{
		"request_loopback_host": "127.0.0.1",
	})

	req, err := http.NewRequest(http.MethodGet, "http://203.0.113.1:65000/probe", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := ops.Request(context.Background(), port, req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok:/probe" {
		t.Fatalf("unexpected body: %q", string(body))
	}
}

func TestClearKeepDirModes(t *testing.T) {
	t.Parallel()

	t.Run("keep dir true", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "workspace")
		ops := mustNewOps(t, infraops.Options{
			"dir":      dir,
			"keep_dir": true,
		})
		if err := ops.Init(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := ops.PutFile(context.Background(), "nested/file.txt", strings.NewReader("x"), 0o644); err != nil {
			t.Fatalf("put file: %v", err)
		}

		if err := ops.Clear(context.Background()); err != nil {
			t.Fatalf("clear: %v", err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("expected directory to remain")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected empty directory, got %d entries", len(entries))
		}
	})

	t.Run("keep dir false", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "workspace")
		ops := mustNewOps(t, infraops.Options{"dir": dir})
		if err := ops.Init(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := ops.PutFile(context.Background(), "file.txt", strings.NewReader("x"), 0o644); err != nil {
			t.Fatalf("put file: %v", err)
		}

		if err := ops.Clear(context.Background()); err != nil {
			t.Fatalf("clear: %v", err)
		}

		_, err := os.Stat(dir)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected directory removed, got %v", err)
		}
	})
}

func TestClearedInstanceBehavior(t *testing.T) {
	t.Parallel()

	ops := initOpsInTempDir(t, nil)
	if err := ops.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if err := ops.Clear(context.Background()); err != nil {
		t.Fatalf("clear idempotent: %v", err)
	}

	if err := ops.Init(context.Background()); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("init after clear expected ErrCleared, got %v", err)
	}
	if _, err := ops.Exec(context.Background(), infraops.ExecCommand{Program: "pwd"}); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("exec after clear expected ErrCleared, got %v", err)
	}
	if err := ops.PutFile(context.Background(), "x.txt", strings.NewReader("x"), 0o644); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("put after clear expected ErrCleared, got %v", err)
	}
	if _, err := ops.GetFile(context.Background(), "x.txt"); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("get after clear expected ErrCleared, got %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/ping", nil)
	if _, err := ops.Request(context.Background(), 8080, req); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("request after clear expected ErrCleared, got %v", err)
	}
	if err := ops.Open(context.Background()); !errors.Is(err, infraops.ErrCleared) {
		t.Fatalf("open after clear expected ErrCleared, got %v", err)
	}
}

func TestOpenExistingNonEmptyDirAllowsExec(t *testing.T) {
	t.Parallel()
	requireProgram(t, "pwd")

	dir := filepath.Join(t.TempDir(), "w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	ops := mustNewOps(t, infraops.Options{"dir": dir})
	if err := ops.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err := ops.Exec(context.Background(), infraops.ExecCommand{Program: "pwd"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

func TestOpenMissingDirReturnsNotInitialized(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nope", "missing")
	ops := mustNewOps(t, infraops.Options{"dir": dir})
	err := ops.Open(context.Background())
	if !errors.Is(err, infraops.ErrNotInitialized) {
		t.Fatalf("Open error = %v, want ErrNotInitialized", err)
	}
}

func TestOpenRejectsSymlinkRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ops := mustNewOps(t, infraops.Options{"dir": link})
	err := ops.Open(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, infraops.ErrInvalidOption) || !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("Open error = %v, want ErrInvalidOption and ErrInvalidDir", err)
	}
}

func TestInitRejectsSymlinkWorkspaceRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ops := mustNewOps(t, infraops.Options{"dir": link})
	err := ops.Init(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, infraops.ErrInvalidOption) || !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("Init error = %v, want ErrInvalidOption and ErrInvalidDir", err)
	}
}

func TestNewRejectsNonLoopbackRequestHost(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "w")
	tests := []struct {
		host    string
		wantErr bool
	}{
		{host: "169.254.169.254", wantErr: true},
		{host: "192.168.1.10", wantErr: true},
		{host: "localhost", wantErr: true},
		{host: "127.0.0.1", wantErr: false},
		{host: "::1", wantErr: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			_, err := New(context.Background(), infraops.Options{"dir": dir, "request_loopback_host": tt.host})
			if tt.wantErr {
				if err == nil || !errors.Is(err, infraops.ErrInvalidOption) {
					t.Fatalf("New() error = %v, want ErrInvalidOption", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
}

func TestClearKeepDirDoesNotFollowReplacedRoot(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	victimFile := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(victimFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	ops := mustNewOps(t, infraops.Options{"dir": dir, "keep_dir": true})
	if err := ops.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := ops.PutFile(context.Background(), "owned.txt", strings.NewReader("owned"), 0o644); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "workspace-moved")); err != nil {
		t.Fatalf("rename workspace: %v", err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatalf("symlink replacement: %v", err)
	}

	if err := ops.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("victim file removed or unreadable: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("victim content = %q, want keep", string(got))
	}
}

func TestClearWithoutKeepDirDoesNotRemoveReplacementContents(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	victimFile := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(victimFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	ops := mustNewOps(t, infraops.Options{"dir": dir, "keep_dir": false})
	if err := ops.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := ops.PutFile(context.Background(), "owned.txt", strings.NewReader("owned"), 0o644); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "workspace-moved")); err != nil {
		t.Fatalf("rename workspace: %v", err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatalf("symlink replacement: %v", err)
	}

	if err := ops.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("victim file removed or unreadable: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("victim content = %q, want keep", string(got))
	}
}

func TestMixedWorkloadConcurrency(t *testing.T) {
	t.Parallel()
	requireProgram(t, "true")

	ops := initOpsInTempDir(t, nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 512)

	for workerID := 0; workerID < 4; workerID++ {
		workerID := workerID

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				name := filepath.Join("data", "w"+strconvItoa(workerID)+".txt")
				content := strings.NewReader(strings.Repeat("x", 512) + "-" + strconvItoa(workerID) + "-" + strconvItoa(i))
				if err := ops.PutFile(ctx, name, content, 0o644); err != nil {
					errCh <- err
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			name := filepath.Join("data", "w"+strconvItoa(workerID)+".txt")
			for i := 0; i < 60; i++ {
				rc, err := ops.GetFile(ctx, name)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					errCh <- err
					return
				}
				_, _ = io.Copy(io.Discard, rc)
				_ = rc.Close()
			}
		}()
	}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ops.Exec(ctx, infraops.ExecCommand{
				Program: "true",
			})
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("mixed workload error: %v", err)
	}
}

func TestNilContextDoesNotPanicOnPublicMethods(t *testing.T) {
	t.Parallel()
	requireProgram(t, "true")

	ops := mustNewOps(t, infraops.Options{"dir": filepath.Join(t.TempDir(), "workspace")})

	mustNotPanic(t, func() {
		if err := ops.Init(nil); err != nil {
			t.Fatalf("Init(nil) error = %v", err)
		}
	})
	mustNotPanic(t, func() {
		if err := ops.Open(nil); err != nil {
			t.Fatalf("Open(nil) error = %v", err)
		}
	})
	mustNotPanic(t, func() {
		if err := ops.PutFile(nil, "x.txt", strings.NewReader("x"), 0o644); err != nil {
			t.Fatalf("PutFile(nil) error = %v", err)
		}
	})
	mustNotPanic(t, func() {
		rc, err := ops.GetFile(nil, "x.txt")
		if err != nil {
			t.Fatalf("GetFile(nil) error = %v", err)
		}
		_ = rc.Close()
	})
	mustNotPanic(t, func() {
		_, err := ops.Exec(nil, infraops.ExecCommand{Program: "true"})
		if err != nil {
			t.Fatalf("Exec(nil) error = %v", err)
		}
	})
	mustNotPanic(t, func() {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/ping", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		_, _ = ops.Request(nil, 9, req)
	})
	mustNotPanic(t, func() {
		if err := ops.Clear(nil); err != nil {
			t.Fatalf("Clear(nil) error = %v", err)
		}
	})
}

func mustNewOps(t *testing.T, opts infraops.Options) infraops.InfraOps {
	t.Helper()

	got, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("new localdir: %v", err)
	}
	return got
}

func initOpsInTempDir(t *testing.T, extra infraops.Options) infraops.InfraOps {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "workspace")
	opts := infraops.Options{"dir": dir}
	for k, v := range extra {
		opts[k] = v
	}

	ops := mustNewOps(t, opts)
	if err := ops.Init(context.Background()); err != nil {
		t.Fatalf("init localdir: %v", err)
	}
	return ops
}

func requireProgram(t *testing.T, program string) {
	t.Helper()
	if _, err := exec.LookPath(program); err != nil {
		t.Skipf("program %q not available", program)
	}
}

func strconvItoa(v int) string {
	return strconv.Itoa(v)
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	fn()
}

type slowReader struct {
	data  []byte
	chunk int
	sleep time.Duration
	pos   int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.chunk <= 0 {
		r.chunk = 1024
	}

	remain := len(r.data) - r.pos
	n := r.chunk
	if n > remain {
		n = remain
	}
	if n > len(p) {
		n = len(p)
	}

	copy(p[:n], r.data[r.pos:r.pos+n])
	r.pos += n
	if r.sleep > 0 {
		time.Sleep(r.sleep)
	}
	return n, nil
}

type gatedReader struct {
	data    []byte
	chunk   int
	pos     int
	started chan struct{}
	allow   chan struct{}
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.chunk <= 0 {
		r.chunk = 1024
	}

	r.once.Do(func() {
		close(r.started)
		<-r.allow
	})

	remain := len(r.data) - r.pos
	n := r.chunk
	if n > remain {
		n = remain
	}
	if n > len(p) {
		n = len(p)
	}

	copy(p[:n], r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}
