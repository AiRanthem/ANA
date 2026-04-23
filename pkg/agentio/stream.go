package agentio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
)

// ChannelStream is a channel-backed EventStream.
type ChannelStream struct {
	ch      <-chan Event
	closeFn func() error
}

func (s *ChannelStream) Recv(ctx context.Context) (*Event, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ev, ok := <-s.ch:
		if !ok {
			return nil, ErrStreamClosed
		}
		return &ev, nil
	}
}

func (s *ChannelStream) Close() error {
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

// NewChannelStream creates an EventStream from a receive-only channel.
func NewChannelStream(ch <-chan Event, closeFn func() error) EventStream {
	return &ChannelStream{ch: ch, closeFn: closeFn}
}

// CollectText consumes text-bearing events into a single string.
func CollectText(ctx context.Context, stream EventStream) (string, error) {
	var b strings.Builder
	defer stream.Close()

	for {
		ev, err := stream.Recv(ctx)
		if errors.Is(err, ErrStreamClosed) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case EventTextDelta, EventMessage, EventStatus:
			if ev.Text != "" {
				b.WriteString(ev.Text)
			}
		case EventFailure:
			if ev.Err != nil {
				return "", errors.New(ev.Err.Message)
			}
			return "", errors.New("stream returned error event without payload")
		}
	}
}

// TextReaderAdapter converts a structured EventStream into an io.ReadCloser by
// concatenating text-bearing events. It is a compatibility layer; the canonical
// abstraction remains EventStream.
type TextReaderAdapter struct {
	stream EventStream
	buf    bytes.Buffer
	closed bool
}

func NewTextReaderAdapter(stream EventStream) io.ReadCloser {
	return &TextReaderAdapter{stream: stream}
}

func (r *TextReaderAdapter) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 && !r.closed {
		ev, err := r.stream.Recv(context.Background())
		if errors.Is(err, ErrStreamClosed) {
			r.closed = true
			break
		}
		if err != nil {
			return 0, err
		}
		switch ev.Type {
		case EventTextDelta, EventMessage, EventStatus:
			r.buf.WriteString(ev.Text)
		case EventFailure:
			if ev.Err != nil {
				return 0, errors.New(ev.Err.Message)
			}
			return 0, errors.New("event error without payload")
		case EventDone:
			r.closed = true
		}
	}
	if r.buf.Len() == 0 && r.closed {
		return 0, io.EOF
	}
	return r.buf.Read(p)
}

func (r *TextReaderAdapter) Close() error {
	r.closed = true
	return r.stream.Close()
}
