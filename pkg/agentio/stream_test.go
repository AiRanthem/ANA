package agentio

import (
	"context"
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
