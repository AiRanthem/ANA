package agentio

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEncodeCanonicalRequestJSON_EncodesTypedInputParts(t *testing.T) {
	req := &InvokeRequest{
		RequestID: "req-1",
		SessionID: "sess-1",
		Role:      RoleUser,
		Metadata:  map[string]string{"source": "test"},
		Parts: []InputPart{
			TextPart{Text: "review this patch"},
			JSONPart{Name: "payload", Data: json.RawMessage(`{"priority":"high"}`)},
			BlobPart{Kind: "image", SourceType: BlobURL, URL: "https://example.com/a.png"},
			BlobPart{Kind: "file", SourceType: BlobFile, Path: "/tmp/result.txt", Filename: "result.txt"},
			BlobPart{Kind: "image", SourceType: BlobInline, Data: []byte("raw-bytes"), Filename: "image.png"},
			ToolResultPart{ToolName: "lint", CallID: "call-1", Data: json.RawMessage(`{"status":"ok"}`)},
		},
	}

	raw, err := EncodeCanonicalRequestJSON(req)
	if err != nil {
		t.Fatalf("EncodeCanonicalRequestJSON() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got["request_id"] != "req-1" {
		t.Fatalf("request_id = %v, want req-1", got["request_id"])
	}
	if got["session_id"] != "sess-1" {
		t.Fatalf("session_id = %v, want sess-1", got["session_id"])
	}
	if got["role"] != string(RoleUser) {
		t.Fatalf("role = %v, want %q", got["role"], RoleUser)
	}

	input, ok := got["input"].([]any)
	if !ok {
		t.Fatalf("input type = %T, want []any", got["input"])
	}
	if len(input) != 6 {
		t.Fatalf("input len = %d, want 6", len(input))
	}

	textPart := mustObject(t, input[0], "input[0]")
	if textPart["type"] != "text" || textPart["text"] != "review this patch" {
		t.Fatalf("text part = %#v, want type/text preserved", textPart)
	}

	jsonPart := mustObject(t, input[1], "input[1]")
	if jsonPart["type"] != "json" || jsonPart["name"] != "payload" {
		t.Fatalf("json part header = %#v, want type/name preserved", jsonPart)
	}
	if !reflect.DeepEqual(jsonPart["data"], map[string]any{"priority": "high"}) {
		t.Fatalf("json part data = %#v, want priority payload", jsonPart["data"])
	}

	urlBlob := mustObject(t, input[2], "input[2]")
	if urlBlob["type"] != "image" || urlBlob["kind"] != "image" {
		t.Fatalf("url blob header = %#v, want image type/kind", urlBlob)
	}
	if source := mustObject(t, urlBlob["source"], "input[2].source"); !reflect.DeepEqual(source, map[string]any{
		"type": "url",
		"url":  "https://example.com/a.png",
	}) {
		t.Fatalf("url blob source = %#v, want url source shape", source)
	}

	fileBlob := mustObject(t, input[3], "input[3]")
	if fileBlob["filename"] != "result.txt" || fileBlob["mime_type"] != "text/plain; charset=utf-8" {
		t.Fatalf("file blob header = %#v, want inferred filename/mime type", fileBlob)
	}
	if source := mustObject(t, fileBlob["source"], "input[3].source"); !reflect.DeepEqual(source, map[string]any{
		"path": "/tmp/result.txt",
		"type": "file",
	}) {
		t.Fatalf("file blob source = %#v, want file source shape", source)
	}

	inlineBlob := mustObject(t, input[4], "input[4]")
	if inlineBlob["filename"] != "image.png" || inlineBlob["mime_type"] != "image/png" {
		t.Fatalf("inline blob header = %#v, want inferred inline filename/mime type", inlineBlob)
	}
	if source := mustObject(t, inlineBlob["source"], "input[4].source"); !reflect.DeepEqual(source, map[string]any{
		"data_base64": base64.StdEncoding.EncodeToString([]byte("raw-bytes")),
		"type":        "inline",
	}) {
		t.Fatalf("inline blob source = %#v, want inline source shape", source)
	}

	toolResult := mustObject(t, input[5], "input[5]")
	if toolResult["type"] != "tool_result" || toolResult["name"] != "lint" {
		t.Fatalf("tool result header = %#v, want type/name preserved", toolResult)
	}
	if !reflect.DeepEqual(toolResult["data"], map[string]any{"status": "ok"}) {
		t.Fatalf("tool result data = %#v, want status payload", toolResult["data"])
	}
	if metadata := mustObject(t, toolResult["metadata"], "input[5].metadata"); !reflect.DeepEqual(metadata, map[string]any{
		"call_id": "call-1",
	}) {
		t.Fatalf("tool result metadata = %#v, want call_id", metadata)
	}
}

func TestEncodeCanonicalRequestJSON_EncodesPointerInputParts(t *testing.T) {
	req := &InvokeRequest{
		RequestID: "req-ptrs",
		Parts: []InputPart{
			&TextPart{Text: "pointer text"},
			&JSONPart{Name: "payload", Data: json.RawMessage(`{"kind":"pointer"}`)},
			&BlobPart{Kind: "file", SourceType: BlobFile, Path: "/tmp/pointer.txt"},
			&ToolResultPart{ToolName: "runner", CallID: "call-ptr", Data: json.RawMessage(`{"ok":true}`)},
		},
	}

	raw, err := EncodeCanonicalRequestJSON(req)
	if err != nil {
		t.Fatalf("EncodeCanonicalRequestJSON() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	input, ok := got["input"].([]any)
	if !ok {
		t.Fatalf("input type = %T, want []any", got["input"])
	}
	if len(input) != 4 {
		t.Fatalf("input len = %d, want 4", len(input))
	}

	textPart := mustObject(t, input[0], "input[0]")
	if textPart["type"] != "text" || textPart["text"] != "pointer text" {
		t.Fatalf("text part = %#v, want pointer text encoding", textPart)
	}

	jsonPart := mustObject(t, input[1], "input[1]")
	if !reflect.DeepEqual(jsonPart["data"], map[string]any{"kind": "pointer"}) {
		t.Fatalf("json part data = %#v, want pointer payload", jsonPart["data"])
	}

	fileBlob := mustObject(t, input[2], "input[2]")
	if source := mustObject(t, fileBlob["source"], "input[2].source"); !reflect.DeepEqual(source, map[string]any{
		"path": "/tmp/pointer.txt",
		"type": "file",
	}) {
		t.Fatalf("file blob source = %#v, want pointer file source shape", source)
	}

	toolResult := mustObject(t, input[3], "input[3]")
	if metadata := mustObject(t, toolResult["metadata"], "input[3].metadata"); !reflect.DeepEqual(metadata, map[string]any{
		"call_id": "call-ptr",
	}) {
		t.Fatalf("tool result metadata = %#v, want pointer call_id", metadata)
	}
}

func TestEncodeCanonicalRequestJSON_EncodesTimeoutAsMilliseconds(t *testing.T) {
	req := &InvokeRequest{
		RequestID: "req-timeout",
		Parts: []InputPart{
			TextPart{Text: "hello"},
		},
		Options: InvokeOptions{
			Model:   "gpt-test",
			Timeout: 1500 * time.Millisecond,
		},
	}

	raw, err := EncodeCanonicalRequestJSON(req)
	if err != nil {
		t.Fatalf("EncodeCanonicalRequestJSON() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	options := mustObject(t, got["options"], "options")
	if options["model"] != "gpt-test" {
		t.Fatalf("options.model = %v, want gpt-test", options["model"])
	}
	if options["timeout_ms"] != float64(1500) {
		t.Fatalf("options.timeout_ms = %v, want 1500", options["timeout_ms"])
	}
	if _, ok := options["timeout"]; ok {
		t.Fatalf("options.timeout present = true, want false")
	}
}

func mustObject(t *testing.T, v any, path string) map[string]any {
	t.Helper()

	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s type = %T, want map[string]any", path, v)
	}
	return obj
}
