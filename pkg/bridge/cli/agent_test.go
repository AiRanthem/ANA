package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

func TestFlattenText(t *testing.T) {
	t.Run("joins supported part types", func(t *testing.T) {
		parts := []agentio.InputPart{
			agentio.TextPart{Text: "hello"},
			agentio.JSONPart{Data: json.RawMessage(`{"kind":"json"}`)},
			agentio.ToolResultPart{
				ToolName: "lookup",
				Data:     json.RawMessage(`{"ok":true}`),
			},
			agentio.BlobPart{Kind: "image", SourceType: agentio.BlobInline, Data: []byte("raw")},
			agentio.BlobPart{Kind: "file", SourceType: agentio.BlobFile, Path: "/tmp/report.txt"},
			agentio.BlobPart{Kind: "image", SourceType: agentio.BlobURL, URL: "https://example.com/image.png"},
		}

		got := flattenText(parts)
		want := "hello\n{\"kind\":\"json\"}\ntool_result:lookup\n{\"ok\":true}\n[image inline]\n[file /tmp/report.txt]\n[image https://example.com/image.png]"
		if got != want {
			t.Fatalf("flattenText() = %q, want %q", got, want)
		}
	})
}

func TestAgentInvoke_EmitsDoneOnlyOnceForDoneFrame(t *testing.T) {
	agent := &Agent{
		Command:      "sh",
		Args:         []string{"-c", "cat >/dev/null; printf '[DONE]\\n'"},
		StdoutFormat: "stream-json",
	}

	stream, err := agent.Invoke(context.Background(), &agentio.InvokeRequest{
		Parts: []agentio.InputPart{agentio.TextPart{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("stream.Close() error = %v", closeErr)
		}
	})

	events := collectCLIEvents(t, stream)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Type != agentio.EventDone {
		t.Fatalf("events[0].Type = %q, want %q", events[0].Type, agentio.EventDone)
	}
}

func TestAgentInvoke_DrainsConcurrentStderrBeforeClosingStream(t *testing.T) {
	agent := &Agent{
		Command:      "sh",
		Args:         []string{"-c", "i=0; while [ $i -lt 2000 ]; do echo err-$i 1>&2; i=$((i+1)); done; printf '[DONE]\\n'"},
		StdoutFormat: "stream-json",
	}

	stream, err := agent.Invoke(context.Background(), &agentio.InvokeRequest{
		Parts: []agentio.InputPart{agentio.TextPart{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("stream.Close() error = %v", closeErr)
		}
	})

	events := collectCLIEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("expected events, got none")
	}

	var stderrCount int
	var doneCount int
	for _, event := range events {
		switch {
		case event.Type == agentio.EventTextDelta && event.Name == "stderr":
			stderrCount++
		case event.Type == agentio.EventDone:
			doneCount++
		}
	}

	if stderrCount == 0 {
		t.Fatal("expected at least one stderr event")
	}
	if doneCount != 1 {
		t.Fatalf("doneCount = %d, want 1", doneCount)
	}
	if got := events[len(events)-1].Type; got != agentio.EventDone {
		t.Fatalf("last event type = %q, want %q", got, agentio.EventDone)
	}
}

func TestAgentInvoke_DoesNotCloseChannelWhileStderrProducerIsBlocked(t *testing.T) {
	agent := &Agent{
		Command:      "sh",
		Args:         []string{"-c", "i=0; while [ $i -lt 1000 ]; do echo err-$i 1>&2; i=$((i+1)); done; printf '[DONE]\\n'"},
		StdoutFormat: "stream-json",
	}

	stream, err := agent.Invoke(context.Background(), &agentio.InvokeRequest{
		Parts: []agentio.InputPart{agentio.TextPart{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("stream.Close() error = %v", closeErr)
		}
	})

	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var events []agentio.Event
	var stderrCount int
	var doneCount int
	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, agentio.ErrStreamClosed) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Recv() error = %v", err)
		}
		events = append(events, *event)
		switch {
		case event.Type == agentio.EventTextDelta && event.Name == "stderr":
			stderrCount++
		case event.Type == agentio.EventDone:
			doneCount++
		}
	}

	if stderrCount == 0 {
		t.Fatal("expected at least one stderr event")
	}
	if doneCount != 1 {
		t.Fatalf("doneCount = %d, want 1", doneCount)
	}
	if got := events[len(events)-1].Type; got != agentio.EventDone {
		t.Fatalf("last event type = %q, want %q", got, agentio.EventDone)
	}
}

func collectCLIEvents(t *testing.T, stream agentio.EventStream) []agentio.Event {
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
