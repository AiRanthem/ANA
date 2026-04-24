package rest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

func TestDefaultHTTPResponseDecoder_UsesSSEStreamForEventStreamResponses(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"Content-Type": []string{"text/event-stream; charset=utf-8"},
		},
		Body: io.NopCloser(strings.NewReader("event: message\ndata: {\"type\":\"text_delta\",\"text\":\"hello\"}\n\n")),
	}

	stream, err := DefaultHTTPResponseDecoder(resp)
	if err != nil {
		t.Fatalf("DefaultHTTPResponseDecoder() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("stream.Close() error = %v", closeErr)
		}
	})

	ctx := context.Background()

	first, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("stream.Recv() first error = %v", err)
	}
	if first.Type != agentio.EventTextDelta {
		t.Fatalf("first event type = %q, want %q", first.Type, agentio.EventTextDelta)
	}
	if first.Text != "hello" {
		t.Fatalf("first event text = %q, want %q", first.Text, "hello")
	}
	if first.Name != "message" {
		t.Fatalf("first event name = %q, want %q", first.Name, "message")
	}

	second, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("stream.Recv() second error = %v", err)
	}
	if second.Type != agentio.EventDone {
		t.Fatalf("second event type = %q, want %q", second.Type, agentio.EventDone)
	}

	_, err = stream.Recv(ctx)
	if !errors.Is(err, agentio.ErrStreamClosed) {
		t.Fatalf("stream.Recv() terminal error = %v, want %v", err, agentio.ErrStreamClosed)
	}
}

func TestDefaultHTTPResponseDecoder_EmitsDoneOnlyOnceForSSEDoneFrame(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
	}

	stream, err := DefaultHTTPResponseDecoder(resp)
	if err != nil {
		t.Fatalf("DefaultHTTPResponseDecoder() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("stream.Close() error = %v", closeErr)
		}
	})

	events := collectEvents(t, stream)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Type != agentio.EventDone {
		t.Fatalf("events[0].Type = %q, want %q", events[0].Type, agentio.EventDone)
	}
}

func collectEvents(t *testing.T, stream agentio.EventStream) []agentio.Event {
	t.Helper()

	ctx := context.Background()
	var events []agentio.Event

	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, agentio.ErrStreamClosed) {
			return events
		}
		if err != nil {
			t.Fatalf("stream.Recv() error = %v", err)
		}
		events = append(events, *event)
	}
}
