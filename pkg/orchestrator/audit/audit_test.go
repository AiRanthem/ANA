package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/orchestrator/idgen"
)

func TestMemorySink_PreservesPerTaskOrder(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	taskID := idgen.TaskID("task-1")

	records := []EventRecord{
		testEvent(taskID, EventTypeTaskCreated, 1),
		testEvent(taskID, EventTypeRouteDirective, 2),
		testEvent(taskID, EventTypeTaskCompleted, 3),
	}
	for _, record := range records {
		if err := sink.WriteEvent(ctx, record); err != nil {
			t.Fatalf("WriteEvent() error = %v", err)
		}
	}

	got := sink.Events(taskID)
	if len(got) != len(records) {
		t.Fatalf("Events() len = %d, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i].Seq != records[i].Seq {
			t.Fatalf("Events()[%d].Seq = %d, want %d", i, got[i].Seq, records[i].Seq)
		}
	}
}

func TestMemorySink_PreservesTranscriptInsertionOrder(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	taskID := idgen.TaskID("task-ordering")

	transcripts := []TranscriptRecord{
		NewOutputTranscript(taskID, "session-1", "request-2", "first", 3, time.Unix(0, 3)),
		NewOutputTranscript(taskID, "session-1", "request-1", "second", 1, time.Unix(0, 1)),
		NewOutputTranscript(taskID, "session-1", "request-1", "third", 2, time.Unix(0, 2)),
	}
	for _, record := range transcripts {
		if err := sink.WriteTranscript(ctx, record); err != nil {
			t.Fatalf("WriteTranscript() error = %v", err)
		}
	}

	got := sink.Transcripts(taskID)
	if len(got) != len(transcripts) {
		t.Fatalf("Transcripts() len = %d, want %d", len(got), len(transcripts))
	}
	wantContent := []string{"first", "second", "third"}
	for i, expected := range wantContent {
		if string(got[i].Content) != expected {
			t.Fatalf("Transcripts()[%d].Content = %q, want %q", i, got[i].Content, expected)
		}
	}
}

func TestMulti_WritesToAllSinks(t *testing.T) {
	ctx := context.Background()
	first := NewMemorySink()
	second := NewMemorySink()
	sink := Multi(first, second)
	record := testEvent("task-1", EventTypeSessionOpened, 1)

	if err := sink.WriteEvent(ctx, record); err != nil {
		t.Fatalf("WriteEvent() error = %v", err)
	}

	if got := first.Events(record.TaskID); len(got) != 1 {
		t.Fatalf("first sink events len = %d, want 1", len(got))
	}
	if got := second.Events(record.TaskID); len(got) != 1 {
		t.Fatalf("second sink events len = %d, want 1", len(got))
	}
}

func TestMulti_FailsFastOnFailingSink(t *testing.T) {
	ctx := context.Background()
	failingErr := errors.New("durable sink unavailable")
	first := NewMemorySink()
	failing := &stubSink{writeEventErr: failingErr}
	third := &stubSink{}
	sink := Multi(first, failing, third)

	err := sink.WriteEvent(ctx, testEvent("task-1", EventTypeRequestFailed, 1))
	if !errors.Is(err, failingErr) {
		t.Fatalf("WriteEvent() error = %v, want %v", err, failingErr)
	}
	if third.writeEventCalls != 0 {
		t.Fatalf("third sink WriteEvent calls = %d, want 0", third.writeEventCalls)
	}
}

func TestMulti_EmptySinkFailsInsteadOfDropping(t *testing.T) {
	ctx := context.Background()
	sink := Multi(nil)

	err := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(err, ErrNoSink) {
		t.Fatalf("WriteEvent() error = %v, want ErrNoSink", err)
	}
}

func TestMulti_CloseWithNoSinksIsNoop(t *testing.T) {
	ctx := context.Background()
	sink := Multi(nil)

	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil for empty Multi", err)
	}
}

func TestMulti_PropagatesFirstFailingTranscriptError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("disk full")
	first := NewMemorySink()
	failing := &stubSink{writeTranscriptErr: wantErr}
	third := &stubSink{}
	sink := Multi(first, failing, third)

	record := TranscriptRecord{
		TaskID:      "task-1",
		SessionID:   "session-1",
		RequestID:   "request-1",
		Kind:        TranscriptKindOutput,
		Content:     []byte("output"),
		ContentType: "text/plain",
		Seq:         1,
		Schema:      SchemaV1,
		CreatedAt:   time.Now(),
	}

	err := sink.WriteTranscript(ctx, record)
	if !errors.Is(err, wantErr) {
		t.Fatalf("WriteTranscript() error = %v, want wrapping %v", err, wantErr)
	}
	if got := first.Transcripts("task-1"); len(got) != 1 {
		t.Fatalf("first sink transcripts len = %d, want 1 (written before failure)", len(got))
	}
	if third.writeTranscriptCalls != 0 {
		t.Fatalf("third sink writeTranscriptCalls = %d, want 0 (Multi must short-circuit on failure)", third.writeTranscriptCalls)
	}
}

func TestInputTranscript_CanStoreFullyRenderedRequestShape(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	content := FullyRenderedRequest{
		TaskID:         "task-1",
		SessionID:      "session-1",
		RequestID:      "request-1",
		WorkspaceID:    "workspace-1",
		WorkspaceAlias: "Alice",
		AgentKey:       "claude-code",
		RuntimeType:    "cli",
		RuntimeKind:    "resumable_cli",
		Inputs: []RenderedInputPart{
			{
				Role:         "system",
				Kind:         "text",
				Content:      "Notes:\n- Bob",
				SizeBytes:    len("Notes:\n- Bob"),
				Redacted:     false,
				RedactionTag: "",
			},
			{
				Role:      "user",
				Kind:      "text",
				Content:   "check stock prices",
				SizeBytes: len("check stock prices"),
			},
		},
		Attachments: []AttachmentMetadata{
			{
				Kind:      "file",
				Name:      "portfolio.csv",
				Reference: "blob://portfolio",
				SizeBytes: 128,
				Digest:    "sha256:abc",
				Redacted:  false,
			},
		},
		PromptDiagnostics: map[string]any{
			"notes_included": true,
		},
		RegistrySnapshot: json.RawMessage(`{"partners":["Bob"]}`),
	}
	record, err := NewInputTranscript(content, 1, time.Now())
	if err != nil {
		t.Fatalf("NewInputTranscript() error = %v", err)
	}
	if err := sink.WriteTranscript(ctx, record); err != nil {
		t.Fatalf("WriteTranscript() error = %v", err)
	}

	got := sink.Transcripts("task-1")
	if len(got) != 1 {
		t.Fatalf("Transcripts() len = %d, want 1", len(got))
	}
	if got[0].Kind != TranscriptKindInput {
		t.Fatalf("Kind = %q, want %q", got[0].Kind, TranscriptKindInput)
	}
	var decoded FullyRenderedRequest
	if err := json.Unmarshal(got[0].Content, &decoded); err != nil {
		t.Fatalf("input transcript content is not JSON: %v", err)
	}
	if decoded.WorkspaceAlias != "Alice" || decoded.RuntimeKind != "resumable_cli" {
		t.Fatalf("decoded workspace/runtime = %q/%q, want Alice/resumable_cli", decoded.WorkspaceAlias, decoded.RuntimeKind)
	}
	if len(decoded.Inputs) != 2 || decoded.Inputs[0].Content != "Notes:\n- Bob" || decoded.Inputs[1].Content != "check stock prices" {
		t.Fatalf("decoded inputs = %#v, want fully rendered ordered inputs", decoded.Inputs)
	}
	if len(decoded.Attachments) != 1 || decoded.Attachments[0].Digest != "sha256:abc" {
		t.Fatalf("decoded attachments = %#v, want digest metadata preserved", decoded.Attachments)
	}
}

func TestRedactionPolicy_PreservesStructuralMetadata(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:         "task-1",
		SessionID:      "session-1",
		RequestID:      "request-1",
		WorkspaceID:    "workspace-1",
		WorkspaceAlias: "Alice",
		AgentKey:       "agent-key",
		RuntimeType:    "cli",
		RuntimeKind:    "resumable_cli",
		Inputs: []RenderedInputPart{
			{Role: "user", Kind: "text", Content: "secret", SizeBytes: 6},
		},
		Attachments: []AttachmentMetadata{
			{Kind: "file", Name: "secret.txt", Reference: "blob://secret", SizeBytes: 10, Digest: "sha256:secret"},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		out := in
		out.Inputs[0].Content = "[redacted]"
		out.Inputs[0].Redacted = true
		out.Inputs[0].RedactionTag = "secret"
		out.Attachments[0].Name = "[redacted]"
		out.Attachments[0].Redacted = true
		out.Attachments[0].RedactionTag = "secret"
		return out, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}

	if redacted.TaskID == "" || redacted.SessionID == "" || redacted.RequestID == "" {
		t.Fatalf("redacted required IDs = %#v, want preserved", redacted)
	}
	if redacted.WorkspaceID != "workspace-1" || redacted.WorkspaceAlias != "Alice" || redacted.RuntimeType != "cli" || redacted.RuntimeKind != "resumable_cli" {
		t.Fatalf("redacted workspace/runtime metadata = %#v, want preserved", redacted)
	}
	if len(redacted.Inputs) != 1 || redacted.Inputs[0].Role != "user" || redacted.Inputs[0].Kind != "text" || redacted.Inputs[0].SizeBytes != 6 {
		t.Fatalf("redacted input structure = %#v, want role/kind/size preserved", redacted.Inputs)
	}
	if len(redacted.Attachments) != 1 || redacted.Attachments[0].Kind != "file" || redacted.Attachments[0].Reference != "blob://secret" || redacted.Attachments[0].Digest != "sha256:secret" {
		t.Fatalf("redacted attachment structure = %#v, want reference/digest preserved", redacted.Attachments)
	}
}

func TestRedactionPolicy_CannotDropStructuralMetadata(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:         "task-1",
		SessionID:      "session-1",
		RequestID:      "request-1",
		WorkspaceID:    "workspace-1",
		WorkspaceAlias: "Alice",
		AgentKey:       "agent-key",
		RuntimeType:    "cli",
		RuntimeKind:    "resumable_cli",
		Inputs: []RenderedInputPart{
			{Role: "system", Kind: "text", Content: "notes", SizeBytes: 5},
			{Role: "user", Kind: "text", Content: "secret", SizeBytes: 6},
		},
		Attachments: []AttachmentMetadata{
			{Kind: "file", Reference: "blob://secret", SizeBytes: 10, Digest: "sha256:secret"},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		out := in
		out.Inputs = out.Inputs[:1]
		out.Attachments = nil
		return out, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_InPlaceMutationCannotCorruptStructuralMetadata(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:         "task-1",
		SessionID:      "session-1",
		RequestID:      "request-1",
		WorkspaceID:    "workspace-1",
		WorkspaceAlias: "Alice",
		AgentKey:       "agent-key",
		RuntimeType:    "cli",
		RuntimeKind:    "resumable_cli",
		Inputs: []RenderedInputPart{
			{
				Role:             "user",
				Kind:             "text",
				Content:          "secret",
				ContentReference: "content://canonical",
				SizeBytes:        6,
				SourceReference:  "message://source",
				Metadata:         map[string]string{"source": "user"},
			},
		},
		Attachments: []AttachmentMetadata{
			{
				Kind:      "file",
				Name:      "secret.txt",
				Reference: "blob://secret",
				SizeBytes: 10,
				Digest:    "sha256:secret",
				Metadata:  map[string]string{"name": "secret.txt"},
			},
		},
		PromptDiagnostics: map[string]any{
			"notes_included": true,
			"nested": map[string]any{
				"steps": []any{"registry", "notes"},
			},
			"typed": []map[string]any{
				{"decision": map[string][]string{"sources": {"registry"}}},
			},
		},
		RegistrySnapshot:  json.RawMessage(`{"partners":["Bob"]}`),
		RegistryReference: "registry://snapshot-1",
		TemplateVersion:   "prompt-v1",
		Metadata:          map[string]string{"request": "root"},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.TaskID = "wrong-task"
		in.Inputs[0].Role = "assistant"
		in.Inputs[0].Kind = "json"
		in.Inputs[0].ContentReference = "content://wrong"
		in.Inputs[0].SizeBytes = 999
		in.Inputs[0].SourceReference = "wrong-source"
		in.Inputs[0].Metadata["source"] = "wrong"
		in.Attachments[0].Kind = "url"
		in.Attachments[0].Reference = "https://wrong"
		in.Attachments[0].SizeBytes = 999
		in.Attachments[0].Digest = "sha256:wrong"
		in.Attachments[0].Metadata["name"] = "wrong"
		in.PromptDiagnostics["notes_included"] = false
		nested := in.PromptDiagnostics["nested"].(map[string]any)
		steps := nested["steps"].([]any)
		steps[0] = "wrong"
		nested["steps"] = steps
		typed := in.PromptDiagnostics["typed"].([]map[string]any)
		decision := typed[0]["decision"].(map[string][]string)
		decision["sources"][0] = "wrong"
		in.RegistrySnapshot = json.RawMessage(`{"partners":["Mallory"]}`)
		in.RegistryReference = "registry://wrong"
		in.TemplateVersion = "prompt-wrong"
		in.Metadata["request"] = "wrong"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}

	if redacted.TaskID != "task-1" || redacted.Inputs[0].Role != "user" || redacted.Inputs[0].Kind != "text" || redacted.Inputs[0].ContentReference != "content://canonical" || redacted.Inputs[0].SizeBytes != 6 || redacted.Inputs[0].SourceReference != "message://source" {
		t.Fatalf("redacted input structure = %#v, want original structural metadata restored", redacted)
	}
	if redacted.Inputs[0].Metadata["source"] != "user" {
		t.Fatalf("redacted input metadata = %#v, want original metadata preserved", redacted.Inputs[0].Metadata)
	}
	if redacted.Attachments[0].Kind != "file" || redacted.Attachments[0].Reference != "blob://secret" || redacted.Attachments[0].SizeBytes != 10 || redacted.Attachments[0].Digest != "sha256:secret" {
		t.Fatalf("redacted attachment structure = %#v, want original structural metadata restored", redacted.Attachments[0])
	}
	nested := redacted.PromptDiagnostics["nested"].(map[string]any)
	steps := nested["steps"].([]any)
	typed := redacted.PromptDiagnostics["typed"].([]map[string]any)
	decision := typed[0]["decision"].(map[string][]string)
	if redacted.Attachments[0].Metadata["name"] != "secret.txt" || redacted.PromptDiagnostics["notes_included"] != false || steps[0] != "wrong" || decision["sources"][0] != "wrong" || string(redacted.RegistrySnapshot) != `{"partners":["Mallory"]}` || redacted.RegistryReference != "registry://snapshot-1" || redacted.TemplateVersion != "prompt-v1" || redacted.Metadata["request"] != "wrong" {
		t.Fatalf("redacted maps/references = input %#v attachment %#v diagnostics %#v snapshot %s reference %q template %q metadata %#v, want policy redactions plus replay references preserved", redacted.Inputs[0].Metadata, redacted.Attachments[0].Metadata, redacted.PromptDiagnostics, redacted.RegistrySnapshot, redacted.RegistryReference, redacted.TemplateVersion, redacted.Metadata)
	}
	originalNested := content.PromptDiagnostics["nested"].(map[string]any)
	originalSteps := originalNested["steps"].([]any)
	originalTyped := content.PromptDiagnostics["typed"].([]map[string]any)
	originalDecision := originalTyped[0]["decision"].(map[string][]string)
	if content.Inputs[0].Role != "user" || content.Inputs[0].ContentReference != "content://canonical" || content.Attachments[0].Digest != "sha256:secret" || originalSteps[0] != "registry" || originalDecision["sources"][0] != "registry" || string(content.RegistrySnapshot) != `{"partners":["Bob"]}` || content.RegistryReference != "registry://snapshot-1" || content.TemplateVersion != "prompt-v1" || content.Metadata["request"] != "root" {
		t.Fatalf("original request mutated: %#v", content)
	}
}

func TestMemorySink_ConcurrentWritesAreSafe(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	taskID := idgen.TaskID("task-1")
	const writers = 32
	const writesPerWriter = 50

	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for seq := range writesPerWriter {
				record := testEvent(taskID, EventTypeRequestTextChunk, uint64(writer*writesPerWriter+seq))
				if err := sink.WriteEvent(ctx, record); err != nil {
					t.Errorf("WriteEvent() error = %v", err)
				}
			}
		}(writer)
	}
	wg.Wait()

	got := sink.Events(taskID)
	if len(got) != writers*writesPerWriter {
		t.Fatalf("Events() len = %d, want %d", len(got), writers*writesPerWriter)
	}
}

func TestMemorySink_CloseRejectsWrites(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	err := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(err, ErrSinkClosed) {
		t.Fatalf("WriteEvent() error = %v, want ErrSinkClosed", err)
	}
}

func testEvent(taskID idgen.TaskID, typ EventType, seq uint64) EventRecord {
	return EventRecord{
		EventID:    fmt.Sprintf("event-%d", seq),
		TaskID:     taskID,
		SessionID:  "session-1",
		RequestID:  "request-1",
		Type:       typ,
		Seq:        seq,
		OccurredAt: time.Unix(0, int64(seq)),
		Payload:    []byte(`{"ok":true}`),
		Schema:     SchemaV1,
	}
}

type stubSink struct {
	writeEventErr        error
	writeTranscriptErr   error
	closeErr             error
	writeEventCalls      int
	writeTranscriptCalls int
}

func (s *stubSink) WriteEvent(ctx context.Context, record EventRecord) error {
	s.writeEventCalls++
	return s.writeEventErr
}

func (s *stubSink) WriteTranscript(ctx context.Context, record TranscriptRecord) error {
	s.writeTranscriptCalls++
	return s.writeTranscriptErr
}

func (s *stubSink) Close(ctx context.Context) error {
	return s.closeErr
}
