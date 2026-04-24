package agentio

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestCollectText_ConcatsTextBearingEvents(t *testing.T) {
	ch := make(chan Event, 4)
	ch <- Event{Type: EventTextDelta, Text: "hello "}
	ch <- Event{Type: EventMessage, Text: "world"}
	ch <- Event{Type: EventDone, At: time.Now()}
	close(ch)

	got, err := CollectText(context.Background(), NewChannelStream(ch, nil))
	if err != nil {
		t.Fatalf("CollectText() error = %v", err)
	}
	if got != "hello world" {
		t.Fatalf("CollectText() = %q, want %q", got, "hello world")
	}
}

func TestCollectText_ReturnsFailureEventMessage(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventFailure, Err: &EventError{Message: "backend failed"}}
	close(ch)

	_, err := CollectText(context.Background(), NewChannelStream(ch, nil))
	if err == nil {
		t.Fatal("CollectText() error = nil, want failure event error")
	}
	if err.Error() != "backend failed" {
		t.Fatalf("CollectText() error = %q, want %q", err.Error(), "backend failed")
	}
}

func TestCollectText_ReturnsEmptyStringWhenStreamClosed(t *testing.T) {
	ch := make(chan Event)
	close(ch)

	got, err := CollectText(context.Background(), NewChannelStream(ch, nil))
	if err != nil {
		t.Fatalf("CollectText() error = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("CollectText() = %q, want empty string", got)
	}
}

func TestTextReaderAdapter_ReadConcatsTextAndReturnsEOF(t *testing.T) {
	ch := make(chan Event, 3)
	ch <- Event{Type: EventTextDelta, Text: "hello "}
	ch <- Event{Type: EventMessage, Text: "world"}
	ch <- Event{Type: EventDone, At: time.Now()}
	close(ch)

	reader := NewTextReaderAdapter(NewChannelStream(ch, nil))
	defer reader.Close()

	all, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if string(all) != "hello world" {
		t.Fatalf("ReadAll() text = %q, want %q", string(all), "hello world")
	}

	buf := make([]byte, 32)
	n, err := reader.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want EOF", err)
	}
	if n != 0 {
		t.Fatalf("Read() n = %d, want 0 at EOF", n)
	}
}

func TestTextReaderAdapter_ReturnsFailureEventMessage(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventFailure, Err: &EventError{Message: "stream failed"}}
	close(ch)

	reader := NewTextReaderAdapter(NewChannelStream(ch, nil))
	defer reader.Close()

	buf := make([]byte, 8)
	_, err := reader.Read(buf)
	if err == nil {
		t.Fatal("Read() error = nil, want failure event error")
	}
	if err.Error() != "stream failed" {
		t.Fatalf("Read() error = %q, want %q", err.Error(), "stream failed")
	}
}

func TestTextReaderAdapter_ReturnsEOFWhenStreamAlreadyClosed(t *testing.T) {
	ch := make(chan Event)
	close(ch)

	reader := NewTextReaderAdapter(NewChannelStream(ch, nil))
	defer reader.Close()

	buf := make([]byte, 4)
	n, err := reader.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want EOF", err)
	}
	if n != 0 {
		t.Fatalf("Read() n = %d, want 0", n)
	}
}

func TestTextReaderAdapter_CloseUnblocksPendingRead(t *testing.T) {
	stream := newBlockingEventStream()
	reader := NewTextReaderAdapter(stream)

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := reader.Read(buf)
		errCh <- err
	}()

	stream.waitForRecv(t)

	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read() remained blocked after Close()")
	}
}

type blockingEventStream struct {
	recvStarted chan struct{}
	closeOnce   sync.Once
	closeCh     chan struct{}
}

func newBlockingEventStream() *blockingEventStream {
	return &blockingEventStream{
		recvStarted: make(chan struct{}),
		closeCh:     make(chan struct{}),
	}
}

func (s *blockingEventStream) Recv(ctx context.Context) (*Event, error) {
	s.closeOnce.Do(func() {
		close(s.recvStarted)
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closeCh:
		return nil, ErrStreamClosed
	}
}

func (s *blockingEventStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.recvStarted)
	})
	select {
	case <-s.closeCh:
	default:
		close(s.closeCh)
	}
	return nil
}

func (s *blockingEventStream) waitForRecv(t *testing.T) {
	t.Helper()

	select {
	case <-s.recvStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Recv() was not called")
	}
}
