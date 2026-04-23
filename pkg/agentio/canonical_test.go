package agentio

import (
	"encoding/json"
	"testing"
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
	input := got["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3", len(input))
	}
}
