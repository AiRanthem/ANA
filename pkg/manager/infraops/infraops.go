package infraops

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"sync"
	"time"
)

// InfraType identifies an infra implementation (for example "localdir").
type InfraType string

// Options is a JSON-serializable options bag passed into a Factory.
type Options map[string]any

// ExecCommand describes a structured command invocation.
type ExecCommand struct {
	Program string
	Args    []string
	Env     []string
	Stdin   io.Reader
	WorkDir string
	Stdout  io.Writer
	Stderr  io.Writer
	Timeout time.Duration
}

// ExecResult reports execution data from Exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

// InfraOps abstracts file/process/network operations for one workspace infra.
type InfraOps interface {
	Type() InfraType
	Dir() string

	// Init prepares a newly-created backing state. Implementations may
	// require the target to be missing or empty and should reject existing
	// non-empty state to protect create idempotency.
	Init(ctx context.Context) error

	// Open attaches this instance to an existing backing state. It must not
	// create or clear state and must not require the state to be empty.
	Open(ctx context.Context) error

	Exec(ctx context.Context, cmd ExecCommand) (ExecResult, error)

	PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error
	GetFile(ctx context.Context, path string) (io.ReadCloser, error)

	Request(ctx context.Context, port int, req *http.Request) (*http.Response, error)

	Clear(ctx context.Context) error
}

// Factory constructs an InfraOps implementation for a workspace.
type Factory func(ctx context.Context, opts Options) (InfraOps, error)

var (
	// ErrInfraTypeUnknown is returned when an infra type has no registered factory.
	ErrInfraTypeUnknown = errors.New("infraops: infra type unknown")
	// ErrInfraTypeConflict is returned when an infra type is registered more than once.
	ErrInfraTypeConflict = errors.New("infraops: infra type conflict")
	// ErrAlreadyInitialized is returned when Init is called on initialized backing state.
	ErrAlreadyInitialized = errors.New("infraops: already initialized")
	// ErrNotInitialized is returned when an operation requires Init to run first.
	ErrNotInitialized = errors.New("infraops: not initialized")
	// ErrInvalidOption is returned when factory options are malformed.
	ErrInvalidOption = errors.New("infraops: invalid option")
	// ErrPathOutsideDir is returned when a relative path escapes Dir.
	ErrPathOutsideDir = errors.New("infraops: path outside dir")
	// ErrNotARegularFile is returned when GetFile targets non-regular files.
	ErrNotARegularFile = errors.New("infraops: not a regular file")
	// ErrUnsupportedRequest is returned when Request is unsupported by an infra.
	ErrUnsupportedRequest = errors.New("infraops: unsupported request")
	// ErrCleared is returned when operations are called on a cleared instance.
	ErrCleared = errors.New("infraops: cleared")
)

// FactorySet stores infra factories by type and is safe for concurrent access.
type FactorySet struct {
	mu sync.RWMutex
	m  map[InfraType]Factory
}

// NewFactorySet constructs an empty factory set.
func NewFactorySet() *FactorySet {
	return &FactorySet{
		m: make(map[InfraType]Factory),
	}
}

// Register adds a factory for infraType or returns ErrInfraTypeConflict.
func (s *FactorySet) Register(infraType InfraType, f Factory) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.m[infraType]; ok {
		return ErrInfraTypeConflict
	}

	s.m[infraType] = f
	return nil
}

// Get returns a registered factory by type.
func (s *FactorySet) Get(infraType InfraType) (Factory, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, ok := s.m[infraType]
	return f, ok
}

// Types returns a sorted snapshot of all registered infra types.
func (s *FactorySet) Types() []InfraType {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]InfraType, 0, len(s.m))
	for infraType := range s.m {
		out = append(out, infraType)
	}

	slices.Sort(out)
	return out
}
