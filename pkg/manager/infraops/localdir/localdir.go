package localdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

const (
	typeLocalDir                 = infraops.InfraType("localdir")
	defaultUmask                 = fs.FileMode(0o022)
	defaultRequestLoopbackHost   = "127.0.0.1"
	execOutputLimit              = 8 << 20
	defaultRequestDialTimeout    = 30 * time.Second
	defaultRequestIdleConnTimout = 30 * time.Second
	getFileReadChunkSize         = 64 << 10 // GetFile read wrapper chunk size (PLAN)
)

// ErrInvalidDir is returned when the localdir "dir" option is invalid.
//
// All ErrInvalidDir returns also wrap infraops.ErrInvalidOption.
var ErrInvalidDir = errors.New("localdir: invalid dir")

// ErrDirNotEmpty is returned when Init is called against a non-empty directory.
var ErrDirNotEmpty = errors.New("localdir: dir not empty")

type options struct {
	dir                 string
	keepDir             bool
	umask               fs.FileMode
	baseEnv             []string
	requestLoopbackHost string
}

type ops struct {
	dir                 string
	keepDir             bool
	umask               fs.FileMode
	baseEnv             []string
	requestLoopbackHost string
	httpClient          *http.Client

	mu          sync.RWMutex
	initialized bool
	cleared     bool
	root        *os.Root

	tmpCounter atomic.Uint64
}

// New constructs the localdir infraops backend from options.
func New(opts infraops.Options) (infraops.InfraOps, error) {
	parsed, err := parseOptions(opts)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout: defaultRequestDialTimeout,
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			IdleConnTimeout:       defaultRequestIdleConnTimout,
			TLSHandshakeTimeout:   defaultRequestDialTimeout,
			ExpectContinueTimeout: time.Second,
		},
	}

	return &ops{
		dir:                 parsed.dir,
		keepDir:             parsed.keepDir,
		umask:               parsed.umask,
		baseEnv:             slices.Clone(parsed.baseEnv),
		requestLoopbackHost: parsed.requestLoopbackHost,
		httpClient:          client,
	}, nil
}

func parseOptions(opts infraops.Options) (options, error) {
	var out options
	if opts == nil {
		opts = infraops.Options{}
	}

	allowed := map[string]struct{}{
		"dir":                   {},
		"keep_dir":              {},
		"umask":                 {},
		"base_env":              {},
		"request_loopback_host": {},
	}
	for key := range opts {
		if _, ok := allowed[key]; !ok {
			return options{}, fmt.Errorf("%w: localdir option %q", infraops.ErrInvalidOption, key)
		}
	}

	rawDir, ok := opts["dir"]
	if !ok {
		return options{}, newInvalidDirError("missing dir option")
	}

	dir, ok := rawDir.(string)
	if !ok {
		return options{}, newInvalidDirError("dir must be a string")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return options{}, newInvalidDirError("dir must not be empty")
	}
	if !filepath.IsAbs(dir) {
		return options{}, newInvalidDirError("dir must be absolute: %q", dir)
	}
	cleanDir := filepath.Clean(dir)
	if isDeniedDir(cleanDir) {
		slog.Warn("localdir.New rejected denied path", "op", "localdir.new", "path", cleanDir)
		return options{}, newInvalidDirError("dir is denied: %q", cleanDir)
	}
	out.dir = cleanDir

	out.keepDir = false
	if rawKeepDir, ok := opts["keep_dir"]; ok {
		keepDir, ok := rawKeepDir.(bool)
		if !ok {
			return options{}, fmt.Errorf("%w: localdir option %q must be bool", infraops.ErrInvalidOption, "keep_dir")
		}
		out.keepDir = keepDir
	}

	out.umask = defaultUmask
	if rawUmask, ok := opts["umask"]; ok {
		umask, err := parseUmask(rawUmask)
		if err != nil {
			return options{}, err
		}
		out.umask = umask
	}

	if rawBaseEnv, ok := opts["base_env"]; ok {
		baseEnv, err := parseBaseEnv(rawBaseEnv)
		if err != nil {
			return options{}, err
		}
		out.baseEnv = baseEnv
	}

	out.requestLoopbackHost = defaultRequestLoopbackHost
	if rawHost, ok := opts["request_loopback_host"]; ok {
		host, ok := rawHost.(string)
		if !ok {
			return options{}, fmt.Errorf("%w: localdir option %q must be string", infraops.ErrInvalidOption, "request_loopback_host")
		}
		loopback, err := parseLoopbackHost(host)
		if err != nil {
			return options{}, err
		}
		out.requestLoopbackHost = loopback
	}

	return out, nil
}

func parseLoopbackHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("%w: localdir option %q must not be empty", infraops.ErrInvalidOption, "request_loopback_host")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: localdir option %q must be a loopback IP", infraops.ErrInvalidOption, "request_loopback_host")
	}
	return host, nil
}

func parseUmask(v any) (fs.FileMode, error) {
	n, ok := toInt64(v)
	if !ok {
		return 0, fmt.Errorf("%w: localdir option %q must be an integer", infraops.ErrInvalidOption, "umask")
	}
	if n < 0 || n > 0o777 {
		return 0, fmt.Errorf("%w: localdir option %q out of range", infraops.ErrInvalidOption, "umask")
	}
	return fs.FileMode(n), nil
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(n), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

func parseBaseEnv(v any) ([]string, error) {
	switch env := v.(type) {
	case []string:
		return slices.Clone(env), nil
	case []any:
		out := make([]string, 0, len(env))
		for idx, item := range env {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%w: localdir option %q[%d] must be string", infraops.ErrInvalidOption, "base_env", idx)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: localdir option %q must be []string", infraops.ErrInvalidOption, "base_env")
	}
}

func isDeniedDir(dir string) bool {
	deny := []string{
		string(filepath.Separator),
		filepath.Clean("/etc"),
		filepath.Clean("/usr"),
		filepath.Clean("/bin"),
		filepath.Clean("/sbin"),
		filepath.Clean("/var/log"),
	}

	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		home = filepath.Clean(home)
		deny = append(deny, home)
		parent := filepath.Dir(home)
		for parent != "" && parent != string(filepath.Separator) && parent != "." {
			deny = append(deny, filepath.Clean(parent))
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
			parent = next
		}
	}

	for _, d := range deny {
		if d == "" {
			continue
		}
		if overlapsPath(dir, d) {
			return true
		}
	}
	return false
}

func overlapsPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)

	root := string(filepath.Separator)
	if a == root || b == root {
		return a == b
	}

	if a == b {
		return true
	}
	if isChildPath(a, b) {
		return true
	}
	if isChildPath(b, a) {
		return true
	}
	return false
}

func isChildPath(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func newInvalidDirError(format string, args ...any) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalidDir, fmt.Sprintf(format, args...), infraops.ErrInvalidOption)
}

func (o *ops) Type() infraops.InfraType {
	return typeLocalDir
}

func (o *ops) Dir() string {
	return o.dir
}

func (o *ops) Init(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.cleared {
		o.mu.Unlock()
		return infraops.ErrCleared
	}
	if o.initialized {
		o.mu.Unlock()
		return infraops.ErrAlreadyInitialized
	}
	o.mu.Unlock()

	parent := filepath.Dir(o.dir)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newInvalidDirError("parent directory does not exist: %q", parent)
		}
		return fmt.Errorf("localdir init stat parent %q: %w", parent, err)
	}
	if !parentInfo.IsDir() {
		return newInvalidDirError("parent is not a directory: %q", parent)
	}

	info, err := os.Lstat(o.dir)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink != 0 {
			return newInvalidDirError("dir must not be a symlink: %q", o.dir)
		}
		if !info.IsDir() {
			return newInvalidDirError("dir is not a directory: %q", o.dir)
		}
		empty, err := dirIsEmpty(o.dir)
		if err != nil {
			return fmt.Errorf("localdir init check empty %q: %w", o.dir, err)
		}
		if !empty {
			return fmt.Errorf("%w: %w: %s", infraops.ErrAlreadyInitialized, ErrDirNotEmpty, o.dir)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(o.dir, 0o755); err != nil {
			return fmt.Errorf("localdir init mkdir %q: %w", o.dir, err)
		}
	default:
		return fmt.Errorf("localdir init stat dir %q: %w", o.dir, err)
	}

	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return fmt.Errorf("localdir init open root %q: %w", o.dir, err)
	}

	if err := o.setRoot(root); err != nil {
		return err
	}
	return nil
}

func (o *ops) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.cleared {
		o.mu.Unlock()
		return infraops.ErrCleared
	}
	if o.initialized {
		o.mu.Unlock()
		return nil
	}
	o.mu.Unlock()

	info, err := os.Lstat(o.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: localdir open missing dir %q", infraops.ErrNotInitialized, o.dir)
		}
		return fmt.Errorf("localdir open stat dir %q: %w", o.dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return newInvalidDirError("dir must not be a symlink: %q", o.dir)
	}
	if !info.IsDir() {
		return newInvalidDirError("dir is not a directory: %q", o.dir)
	}

	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return fmt.Errorf("localdir open root %q: %w", o.dir, err)
	}
	return o.setRoot(root)
}

func (o *ops) setRoot(root *os.Root) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cleared {
		_ = root.Close()
		return infraops.ErrCleared
	}
	if o.initialized {
		_ = root.Close()
		return nil
	}
	o.root = root
	o.initialized = true
	return nil
}

func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func (o *ops) Exec(ctx context.Context, cmd infraops.ExecCommand) (infraops.ExecResult, error) {
	root, err := o.readyRoot()
	if err != nil {
		return infraops.ExecResult{}, err
	}

	if strings.TrimSpace(cmd.Program) == "" {
		return infraops.ExecResult{}, fmt.Errorf("%w: localdir exec program must not be empty", infraops.ErrInvalidOption)
	}

	workDir, err := o.resolveWorkDir(root, cmd.WorkDir)
	if err != nil {
		return infraops.ExecResult{}, err
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if cmd.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	execCmd := exec.CommandContext(runCtx, cmd.Program, cmd.Args...)
	execCmd.Dir = workDir

	baseEnv := os.Environ()
	execCmd.Env = make([]string, 0, len(baseEnv)+len(o.baseEnv)+len(cmd.Env))
	execCmd.Env = append(execCmd.Env, baseEnv...)
	execCmd.Env = append(execCmd.Env, o.baseEnv...)
	execCmd.Env = append(execCmd.Env, cmd.Env...)
	execCmd.Stdin = cmd.Stdin

	stdoutBuf := newCappedBuffer(execOutputLimit)
	stderrBuf := newCappedBuffer(execOutputLimit)

	if cmd.Stdout != nil {
		execCmd.Stdout = cmd.Stdout
	} else {
		execCmd.Stdout = stdoutBuf
	}
	if cmd.Stderr != nil {
		execCmd.Stderr = cmd.Stderr
	} else {
		execCmd.Stderr = stderrBuf
	}

	start := time.Now()
	runErr := execCmd.Run()
	result := infraops.ExecResult{
		Duration: time.Since(start),
	}
	if cmd.Stdout == nil {
		result.Stdout = stdoutBuf.Bytes()
	}
	if cmd.Stderr == nil {
		result.Stderr = stderrBuf.Bytes()
	}

	if stdoutBuf.Truncated() {
		slog.Warn("localdir exec stdout truncated", "op", "localdir.exec", "program", cmd.Program, "bytes", len(result.Stdout))
	}
	if stderrBuf.Truncated() {
		slog.Warn("localdir exec stderr truncated", "op", "localdir.exec", "program", cmd.Program, "bytes", len(result.Stderr))
	}

	if runErr == nil {
		return result, nil
	}
	if err := runCtx.Err(); err != nil {
		return infraops.ExecResult{}, err
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return infraops.ExecResult{}, fmt.Errorf("localdir exec run %q: %w", cmd.Program, runErr)
}

func (o *ops) resolveWorkDir(root *os.Root, workDir string) (string, error) {
	if workDir == "" {
		return o.dir, nil
	}
	if isPathOutsideRootLexical(workDir) {
		return "", infraops.ErrPathOutsideDir
	}
	if filepath.Clean(workDir) == "." {
		return o.dir, nil
	}

	subRoot, err := root.OpenRoot(workDir)
	if err != nil {
		if isPathEscapeError(err) {
			return "", infraops.ErrPathOutsideDir
		}
		return "", fmt.Errorf("localdir exec validate workdir %q: %w", workDir, err)
	}
	_ = subRoot.Close()

	return filepath.Join(o.dir, filepath.Clean(workDir)), nil
}

func (o *ops) PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error {
	root, err := o.readyRoot()
	if err != nil {
		return err
	}
	cleanPath, err := sanitizeRelativePath(path)
	if err != nil {
		return err
	}

	parent := filepath.Dir(cleanPath)
	parentPerm := fs.FileMode(0o755) &^ o.umask
	if err := root.MkdirAll(parent, parentPerm); err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return fmt.Errorf("localdir putfile mkdir %q: %w", parent, err)
	}

	tmpPath := o.tempSiblingPath(cleanPath)
	filePerm := mode &^ o.umask
	tmpFile, err := root.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return fmt.Errorf("localdir putfile create temp %q: %w", tmpPath, err)
	}
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = root.Remove(tmpPath)
		}
	}()

	reader := &ctxReadCloser{ctx: ctx, rc: io.NopCloser(content)}
	if _, err := io.Copy(tmpFile, reader); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("localdir putfile copy %q: %w", path, err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("localdir putfile sync temp %q: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("localdir putfile close temp %q: %w", tmpPath, err)
	}

	if err := root.Rename(tmpPath, cleanPath); err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return fmt.Errorf("localdir putfile rename %q -> %q: %w", tmpPath, cleanPath, err)
	}

	if err := syncRootDir(root, parent); err != nil {
		return fmt.Errorf("localdir putfile fsync parent %q: %w", parent, err)
	}

	cleanupTemp = false
	return nil
}

func syncRootDir(root *os.Root, dir string) error {
	if dir == "" {
		dir = "."
	}
	df, err := root.Open(dir)
	if err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return err
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return err
	}
	return nil
}

func (o *ops) tempSiblingPath(path string) string {
	base := filepath.Base(path)
	parent := filepath.Dir(path)
	id := o.tmpCounter.Add(1)
	name := "." + base + ".tmp." + strconv.FormatInt(time.Now().UnixNano(), 10) + "." + strconv.FormatUint(id, 10)
	return filepath.Join(parent, name)
}

func (o *ops) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	root, err := o.readyRoot()
	if err != nil {
		return nil, err
	}
	cleanPath, err := sanitizeRelativePath(path)
	if err != nil {
		return nil, err
	}

	f, err := root.Open(cleanPath)
	if err != nil {
		if isPathEscapeError(err) {
			return nil, infraops.ErrPathOutsideDir
		}
		return nil, fmt.Errorf("localdir getfile open %q: %w", cleanPath, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("localdir getfile stat %q: %w", cleanPath, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, infraops.ErrNotARegularFile
	}

	return &ctxReadCloser{
		ctx: ctx,
		rc:  f,
	}, nil
}

func (o *ops) Request(ctx context.Context, port int, req *http.Request) (*http.Response, error) {
	if _, err := o.readyRoot(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("%w: localdir request nil request", infraops.ErrInvalidOption)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: localdir request invalid port %d", infraops.ErrInvalidOption, port)
	}
	if req.URL == nil {
		return nil, fmt.Errorf("%w: localdir request nil URL", infraops.ErrInvalidOption)
	}

	cloned := req.Clone(ctx)
	cloned.URL.Scheme = "http"
	cloned.URL.Host = net.JoinHostPort(o.requestLoopbackHost, strconv.Itoa(port))
	cloned.Host = ""

	resp, err := o.httpClient.Do(cloned)
	if err != nil {
		return nil, fmt.Errorf("infraops request: %w", err)
	}
	return resp, nil
}

func (o *ops) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.cleared {
		o.mu.Unlock()
		return nil
	}
	o.cleared = true
	root := o.root
	o.root = nil
	o.initialized = false
	o.mu.Unlock()

	if root == nil {
		pathInfo, err := os.Lstat(o.dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("localdir clear lstat dir %q: %w", o.dir, err)
		}
		if pathInfo.Mode()&fs.ModeSymlink != 0 {
			if o.keepDir {
				return nil
			}
			if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("localdir clear remove symlink %q: %w", o.dir, err)
			}
			return nil
		}
		if !pathInfo.IsDir() {
			if o.keepDir {
				return newInvalidDirError("dir is not a directory: %q", o.dir)
			}
			if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("localdir clear remove non-dir %q: %w", o.dir, err)
			}
			return nil
		}
		opened, err := os.OpenRoot(o.dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("localdir clear open root %q: %w", o.dir, err)
		}
		root = opened
	}
	defer root.Close()

	rootInfo, statErr := root.Stat(".")
	if statErr != nil {
		return fmt.Errorf("localdir clear stat root %q: %w", o.dir, statErr)
	}
	if err := clearRootContents(ctx, root); err != nil {
		return fmt.Errorf("localdir clear contents %q: %w", o.dir, err)
	}
	if o.keepDir {
		return nil
	}

	pathInfo, err := os.Lstat(o.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localdir clear lstat dir %q: %w", o.dir, err)
	}
	if !os.SameFile(rootInfo, pathInfo) {
		// The path was replaced after the root was opened. Contents were cleared
		// through the safe root handle; do not remove the replacement path.
		return nil
	}
	if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localdir clear remove dir %q: %w", o.dir, err)
	}
	return nil
}

func clearRootContents(ctx context.Context, root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			if isPathEscapeError(err) {
				return infraops.ErrPathOutsideDir
			}
			return err
		}
	}
	return nil
}

func (o *ops) readyRoot() (*os.Root, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.cleared {
		return nil, infraops.ErrCleared
	}
	if !o.initialized || o.root == nil {
		return nil, infraops.ErrNotInitialized
	}
	return o.root, nil
}

func sanitizeRelativePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", infraops.ErrPathOutsideDir
	}
	if isPathOutsideRootLexical(path) {
		return "", infraops.ErrPathOutsideDir
	}
	return filepath.Clean(path), nil
}

func isPathOutsideRootLexical(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	clean := filepath.Clean(path)
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func isPathEscapeError(err error) bool {
	return strings.Contains(err.Error(), "path escapes from parent")
}

type ctxReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
}

func (r *ctxReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for total < len(p) {
		select {
		case <-r.ctx.Done():
			return total, r.ctx.Err()
		default:
		}
		remain := len(p) - total
		chunk := remain
		if chunk > getFileReadChunkSize {
			chunk = getFileReadChunkSize
		}
		n, err := r.rc.Read(p[total : total+chunk])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

func (r *ctxReadCloser) Close() error {
	return r.rc.Close()
}

type cappedBuffer struct {
	max       int
	truncated bool
	buf       bytes.Buffer
}

func newCappedBuffer(max int) *cappedBuffer {
	return &cappedBuffer{max: max}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.max <= 0 {
		b.truncated = true
		return len(p), nil
	}

	remain := b.max - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) <= remain {
		_, _ = b.buf.Write(p)
		return len(p), nil
	}
	_, _ = b.buf.Write(p[:remain])
	b.truncated = true
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *cappedBuffer) Truncated() bool {
	return b.truncated
}
