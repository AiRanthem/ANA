package socket

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

// JSONSocket is a transport abstraction for full-duplex JSON message streams.
type JSONSocket interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, payload []byte) error
	Close() error
}

// Dialer opens a JSONSocket for a request/session.
type Dialer interface {
	Dial(ctx context.Context, req *agentio.InvokeRequest) (JSONSocket, error)
}

// JSONSocketDialer preserves the old transport-specific type name within the subpackage.
type JSONSocketDialer = Dialer

// Agent is a session-capable socket transport adapter.
type Agent struct {
	NameStr string
	Dialer  Dialer

	EncodeFrame func(parts []agentio.InputPart) ([]byte, error)
	DecodeFrame func(payload []byte) ([]agentio.Event, error)
}

// SocketAgent preserves the old transport-specific type name within the subpackage.
type SocketAgent = Agent

func (a *Agent) Name() string {
	if a.NameStr == "" {
		return "socket-agent"
	}
	return a.NameStr
}

func (a *Agent) Invoke(ctx context.Context, req *agentio.InvokeRequest) (agentio.EventStream, error) {
	session, err := a.OpenSession(ctx, req)
	if err != nil {
		return nil, err
	}
	return &sessionStream{session: session}, nil
}

func (a *Agent) OpenSession(ctx context.Context, req *agentio.InvokeRequest) (agentio.Session, error) {
	if a.Dialer == nil {
		return nil, errors.New("socket agent missing dialer")
	}
	if req == nil {
		return nil, errors.New("nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	conn, err := a.Dialer.Dial(ctx, req)
	if err != nil {
		return nil, err
	}

	encode := a.EncodeFrame
	if encode == nil {
		encode = func(parts []agentio.InputPart) ([]byte, error) {
			tmp := *req
			tmp.Parts = parts
			return agentio.EncodeCanonicalRequestJSON(&tmp)
		}
	}

	decode := a.DecodeFrame
	if decode == nil {
		decode = DefaultFrameDecoder
	}

	session := &socketSession{
		id:     req.SessionID,
		conn:   conn,
		encode: encode,
		decode: decode,
		ch:     make(chan agentio.Event, 128),
	}
	go session.readLoop(ctx)

	if len(req.Parts) > 0 {
		if err := session.Send(ctx, req.Parts...); err != nil {
			_ = session.Close()
			return nil, err
		}
	}

	return session, nil
}

type sessionStream struct {
	session agentio.Session
}

func (s *sessionStream) Recv(ctx context.Context) (*agentio.Event, error) { return s.session.Recv(ctx) }
func (s *sessionStream) Close() error                                     { return s.session.Close() }

type socketSession struct {
	id     string
	conn   JSONSocket
	encode func([]agentio.InputPart) ([]byte, error)
	decode func([]byte) ([]agentio.Event, error)

	ch        chan agentio.Event
	closeOnce sync.Once
}

func (s *socketSession) ID() string { return s.id }

func (s *socketSession) Send(ctx context.Context, parts ...agentio.InputPart) error {
	payload, err := s.encode(parts)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, payload)
}

func (s *socketSession) Recv(ctx context.Context) (*agentio.Event, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case event, ok := <-s.ch:
		if !ok {
			return nil, agentio.ErrStreamClosed
		}
		return &event, nil
	}
}

func (s *socketSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.conn.Close()
		close(s.ch)
	})
	return err
}

func (s *socketSession) readLoop(ctx context.Context) {
	defer func() {
		_ = s.conn.Close()
		s.closeOnce.Do(func() { close(s.ch) })
	}()

	for {
		payload, err := s.conn.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				select {
				case s.ch <- agentio.Event{
					Type: agentio.EventFailure,
					Err: &agentio.EventError{
						Code:    "socket_read_error",
						Message: err.Error(),
					},
					At: time.Now(),
				}:
				default:
				}
			}
			return
		}

		events, err := s.decode(payload)
		if err != nil {
			select {
			case s.ch <- agentio.Event{
				Type: agentio.EventFailure,
				Err: &agentio.EventError{
					Code:    "socket_decode_error",
					Message: err.Error(),
				},
				At: time.Now(),
			}:
			default:
			}
			return
		}

		for _, event := range events {
			select {
			case <-ctx.Done():
				return
			case s.ch <- event:
			}
		}
	}
}

// DefaultFrameDecoder decodes a single JSON payload into one or more canonical events.
func DefaultFrameDecoder(payload []byte) ([]agentio.Event, error) {
	var arr []map[string]any
	if err := json.Unmarshal(payload, &arr); err == nil {
		out := make([]agentio.Event, 0, len(arr))
		for _, item := range arr {
			out = append(out, bestEffortEventFromMap(item))
		}
		return out, nil
	}

	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		text := strings.TrimSpace(string(payload))
		if text == "" {
			return nil, nil
		}
		return []agentio.Event{{Type: agentio.EventTextDelta, Text: text, At: time.Now()}}, nil
	}

	return []agentio.Event{bestEffortEventFromMap(obj)}, nil
}

func bestEffortEventFromMap(m map[string]any) agentio.Event {
	event := agentio.Event{
		Type:     agentio.EventStructured,
		Metadata: map[string]any{},
		At:       time.Now(),
	}

	if eventType, ok := stringField(m, "type"); ok {
		event.Type = agentio.EventType(eventType)
	}
	if text, ok := firstStringField(m, "text", "delta", "output_text", "message"); ok {
		event.Text = text
		if event.Type == agentio.EventStructured {
			event.Type = agentio.EventTextDelta
		}
	}
	if name, ok := firstStringField(m, "name", "event"); ok {
		event.Name = name
	}
	if usage, ok := m["usage"]; ok {
		if raw, err := json.Marshal(usage); err == nil {
			var decoded agentio.Usage
			if json.Unmarshal(raw, &decoded) == nil {
				event.Usage = &decoded
				if event.Type == agentio.EventStructured {
					event.Type = agentio.EventUsage
				}
			}
		}
	}
	if rawErr, ok := m["error"]; ok {
		switch typed := rawErr.(type) {
		case string:
			event.Err = &agentio.EventError{Message: typed}
		default:
			if raw, err := json.Marshal(typed); err == nil {
				var decoded agentio.EventError
				if json.Unmarshal(raw, &decoded) == nil && decoded.Message != "" {
					event.Err = &decoded
				} else {
					event.Err = &agentio.EventError{Message: string(raw)}
				}
			}
		}
		if event.Err != nil {
			event.Type = agentio.EventFailure
		}
	}
	if event.Text == "" {
		if raw, err := json.Marshal(m); err == nil {
			event.JSON = raw
		}
	}

	return event
}

func stringField(m map[string]any, key string) (string, bool) {
	value, ok := m[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func firstStringField(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if text, ok := stringField(m, key); ok && text != "" {
			return text, true
		}
	}
	return "", false
}
