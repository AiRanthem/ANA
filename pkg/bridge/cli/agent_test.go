package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
