package agentio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	// ErrStreamClosed is returned when an EventStream has no more events.
	ErrStreamClosed = io.EOF
)

// Role represents the semantic source of a request or message.
type Role string

const (
	RoleUser      Role = "user"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	RoleAssistant Role = "assistant"
)

// BlobSourceType describes where a binary payload should be sourced from.
type BlobSourceType string

const (
	BlobInline BlobSourceType = "inline"
	BlobFile   BlobSourceType = "file"
	BlobURL    BlobSourceType = "url"
)

// EventType is the canonical event type surfaced by adapters.
type EventType string

const (
	EventTextDelta  EventType = "text_delta"
	EventMessage    EventType = "message"
	EventStructured EventType = "structured"
	EventBinary     EventType = "binary"
	EventStatus     EventType = "status"
	EventUsage      EventType = "usage"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventFailure    EventType = "error"
	EventDone       EventType = "done"
)

// InputPart is the canonical input unit. Adapters translate these parts into
// protocol-specific payloads such as REST JSON, JSON-RPC, protobuf, stdin JSON,
// multipart uploads, or session frames.
type InputPart interface {
	inputPartTag()
	PartType() string
}

// TextPart carries plain text.
type TextPart struct {
	Text string `json:"text"`
}

func (TextPart) inputPartTag()    {}
func (TextPart) PartType() string { return "text" }

func (p TextPart) Validate() error {
	if strings.TrimSpace(p.Text) == "" {
		return errors.New("text part is empty")
	}
	return nil
}

func (p TextPart) String() string { return p.Text }

// JSONPart carries protocol-agnostic structured data that is still considered
// user/tool input rather than adapter metadata.
type JSONPart struct {
	Name string          `json:"name,omitempty"`
	Data json.RawMessage `json:"data"`
}

func (JSONPart) inputPartTag()    {}
func (JSONPart) PartType() string { return "json" }

func (p JSONPart) Validate() error {
	if len(p.Data) == 0 {
		return errors.New("json part is empty")
	}
	return nil
}

// BlobPart carries multimodal or file inputs. Use inline bytes only for small
// payloads. Prefer Path or URL for large media.
type BlobPart struct {
	Kind       string            `json:"kind"`
	MIMEType   string            `json:"mime_type,omitempty"`
	Filename   string            `json:"filename,omitempty"`
	SourceType BlobSourceType    `json:"source_type"`
	Data       []byte            `json:"-"`
	Path       string            `json:"path,omitempty"`
	URL        string            `json:"url,omitempty"`
	Size       int64             `json:"size,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

func (BlobPart) inputPartTag()    {}
func (BlobPart) PartType() string { return "blob" }

func (p BlobPart) Validate() error {
	if strings.TrimSpace(p.Kind) == "" {
		return errors.New("blob kind is required")
	}
	switch p.SourceType {
	case BlobInline:
		if len(p.Data) == 0 {
			return errors.New("inline blob has empty data")
		}
	case BlobFile:
		if strings.TrimSpace(p.Path) == "" {
			return errors.New("file blob has empty path")
		}
	case BlobURL:
		if strings.TrimSpace(p.URL) == "" {
			return errors.New("url blob has empty url")
		}
	default:
		return fmt.Errorf("unsupported blob source type: %q", p.SourceType)
	}
	return nil
}

// ToolResultPart is the canonical way to feed tool output back into a
// stateful agent session.
type ToolResultPart struct {
	ToolName string          `json:"tool_name"`
	CallID   string          `json:"call_id,omitempty"`
	Data     json.RawMessage `json:"data"`
}

func (ToolResultPart) inputPartTag()    {}
func (ToolResultPart) PartType() string { return "tool_result" }

func (p ToolResultPart) Validate() error {
	if strings.TrimSpace(p.ToolName) == "" {
		return errors.New("tool result missing tool name")
	}
	if len(p.Data) == 0 {
		return errors.New("tool result missing data")
	}
	return nil
}

// InvokeOptions capture generic execution controls. Protocol-specific extras go
// into Extra.
type InvokeOptions struct {
	Model          string         `json:"model,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	Timeout        time.Duration  `json:"timeout,omitempty"`
	MaxOutputBytes int64          `json:"max_output_bytes,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	Extra          map[string]any `json:"extra,omitempty"`
}

// InvokeRequest is the normalized request passed to an Agent.
type InvokeRequest struct {
	RequestID string            `json:"request_id,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Role      Role              `json:"role,omitempty"`
	Parts     []InputPart       `json:"-"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Options   InvokeOptions     `json:"options,omitempty"`
}

func (r *InvokeRequest) Validate() error {
	if len(r.Parts) == 0 {
		return errors.New("request has no input parts")
	}
	for i, part := range r.Parts {
		switch p := part.(type) {
		case TextPart:
			if err := p.Validate(); err != nil {
				return fmt.Errorf("parts[%d]: %w", i, err)
			}
		case JSONPart:
			if err := p.Validate(); err != nil {
				return fmt.Errorf("parts[%d]: %w", i, err)
			}
		case BlobPart:
			if err := p.Validate(); err != nil {
				return fmt.Errorf("parts[%d]: %w", i, err)
			}
		case ToolResultPart:
			if err := p.Validate(); err != nil {
				return fmt.Errorf("parts[%d]: %w", i, err)
			}
		default:
			return fmt.Errorf("parts[%d]: unsupported part type %T", i, part)
		}
	}
	return nil
}

// Usage is a canonical token/cost accounting payload.
type Usage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// EventError is surfaced as Event.Type == EventFailure.
type EventError struct {
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Event is the canonical output unit. All adapters should translate transport-
// specific frames into this structure.
type Event struct {
	Type     EventType       `json:"type"`
	Text     string          `json:"text,omitempty"`
	Name     string          `json:"name,omitempty"`
	Data     []byte          `json:"data,omitempty"`
	JSON     json.RawMessage `json:"json,omitempty"`
	Usage    *Usage          `json:"usage,omitempty"`
	Err      *EventError     `json:"error,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	At       time.Time       `json:"at,omitempty"`
}

// EventStream is the canonical receive side used by the web/API layer.
type EventStream interface {
	Recv(ctx context.Context) (*Event, error)
	Close() error
}

// Agent is a one-shot request/response abstraction.
type Agent interface {
	Name() string
	Invoke(ctx context.Context, req *InvokeRequest) (EventStream, error)
}

// Session is for full-duplex or multi-turn transports such as WebSocket, JSON-
// RPC over stdio, or long-running remote agents.
type Session interface {
	ID() string
	Send(ctx context.Context, parts ...InputPart) error
	Recv(ctx context.Context) (*Event, error)
	Close() error
}

// SessionAgent can open a long-lived session. Stateless REST adapters may
// implement only Agent, while WebSocket/MCP-style adapters should usually
// implement SessionAgent as well.
type SessionAgent interface {
	Name() string
	OpenSession(ctx context.Context, req *InvokeRequest) (Session, error)
}
