package socket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

func TestAgentOpenSession_ReceivesDecodedEvent(t *testing.T) {
	sock := newScriptedSocket()
	agent := &Agent{
		Dialer: dialerFunc(func(context.Context, *agentio.InvokeRequest) (JSONSocket, error) {
			return sock, nil
		}),
	}

	session, err := agent.OpenSession(context.Background(), &agentio.InvokeRequest{
		SessionID: "sess-1",
		Parts:     []agentio.InputPart{agentio.TextPart{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := session.Close(); closeErr != nil {
			t.Fatalf("session.Close() error = %v", closeErr)
		}
	})

	sock.enqueue(readResult{payload: []byte(`{"type":"text_delta","text":"world"}`)})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("session.Recv() error = %v", err)
	}
	if event.Type != agentio.EventTextDelta {
		t.Fatalf("event.Type = %q, want %q", event.Type, agentio.EventTextDelta)
	}
	if event.Text != "world" {
		t.Fatalf("event.Text = %q, want %q", event.Text, "world")
	}
}

func TestSessionCloseWhileRead_DoesNotPanicAndClosesRecv(t *testing.T) {
	sock := newScriptedSocket()
	agent := &Agent{
		Dialer: dialerFunc(func(context.Context, *agentio.InvokeRequest) (JSONSocket, error) {
			return sock, nil
		}),
	}

	session, err := agent.OpenSession(context.Background(), &agentio.InvokeRequest{
		SessionID: "sess-2",
		Parts:     []agentio.InputPart{agentio.TextPart{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err = session.Recv(ctx)
	if !errors.Is(err, agentio.ErrStreamClosed) {
		t.Fatalf("session.Recv() error = %v, want %v", err, agentio.ErrStreamClosed)
	}

	sock.enqueue(readResult{payload: []byte(`{"type":"text_delta","text":"late"}`)})
	sock.enqueue(readResult{err: context.Canceled})

	time.Sleep(50 * time.Millisecond)
}

type dialerFunc func(context.Context, *agentio.InvokeRequest) (JSONSocket, error)

func (f dialerFunc) Dial(ctx context.Context, req *agentio.InvokeRequest) (JSONSocket, error) {
	return f(ctx, req)
}

type readResult struct {
	payload []byte
	err     error
}

type scriptedSocket struct {
	mu     sync.Mutex
	reads  chan readResult
	writes [][]byte
	closed bool
}

func newScriptedSocket() *scriptedSocket {
	return &scriptedSocket{
		reads: make(chan readResult, 8),
	}
}

func (s *scriptedSocket) enqueue(result readResult) {
	s.reads <- result
}

func (s *scriptedSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-s.reads:
		return result.payload, result.err
	}
}

func (s *scriptedSocket) Write(_ context.Context, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, append([]byte(nil), payload...))
	return nil
}

func (s *scriptedSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
