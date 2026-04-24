package cli

import (
	"encoding/json"
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
