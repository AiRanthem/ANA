package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func TestMulti_EmptySinkTranscriptFailsInsteadOfDropping(t *testing.T) {
	ctx := context.Background()
	sink := Multi(nil)

	err := sink.WriteTranscript(ctx, NewOutputTranscript("task-1", "session-1", "request-1", "output", 1, time.Now()))
	if !errors.Is(err, ErrNoSink) {
		t.Fatalf("WriteTranscript() error = %v, want ErrNoSink", err)
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

func TestMulti_ConcurrentWritesCannotInterleaveFanout(t *testing.T) {
	ctx := context.Background()
	first := newBlockingFanoutSink()
	second := &recordingSink{}
	sink := Multi(first, second)

	firstDone := make(chan error, 1)
	go func() {
		record := testEvent("task-1", EventTypeRequestRunning, 1)
		err := sink.WriteEvent(ctx, record)
		firstDone <- err
	}()
	if err := first.waitForFirstCall(); err != nil {
		t.Fatal(err)
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		record := testEvent("task-1", EventTypeRequestTextChunk, 2)
		secondDone <- sink.WriteEvent(ctx, record)
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second write goroutine to start")
	}

	select {
	case <-first.secondCallStarted:
		t.Fatal("second write reached the first underlying sink before the first write completed fan-out")
	case <-time.After(20 * time.Millisecond):
	}
	if got := second.eventSeqs(); len(got) != 0 {
		t.Fatalf("second sink event seqs = %v, want none before first write is released", got)
	}

	first.releaseFirstCall()
	if err := <-firstDone; err != nil {
		t.Fatalf("first WriteEvent() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second WriteEvent() error = %v", err)
	}

	if got := second.eventSeqs(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("second sink event seqs = %v, want [1 2]", got)
	}
}

func TestMulti_CloseClosesAllSinksAndJoinsErrors(t *testing.T) {
	ctx := context.Background()
	firstErr := errors.New("first close failed")
	thirdErr := errors.New("third close failed")
	first := &stubSink{closeErr: firstErr}
	second := &stubSink{}
	third := &stubSink{closeErr: thirdErr}
	sink := Multi(first, second, third)

	err := sink.Close(ctx)
	if !errors.Is(err, firstErr) || !errors.Is(err, thirdErr) {
		t.Fatalf("Close() error = %v, want both close errors", err)
	}
	if first.closeCalls != 1 || second.closeCalls != 1 || third.closeCalls != 1 {
		t.Fatalf("close calls = %d/%d/%d, want 1/1/1", first.closeCalls, second.closeCalls, third.closeCalls)
	}
}

func TestMulti_CloseRetriesFailedChildrenUntilAllClose(t *testing.T) {
	ctx := context.Background()
	closeErr := errors.New("close failed")
	first := &stubSink{}
	second := &stubSink{closeErr: closeErr}
	third := &stubSink{}
	sink := Multi(first, second, third)

	err := sink.Close(ctx)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, closeErr)
	}
	if first.closeCalls != 1 || second.closeCalls != 1 || third.closeCalls != 1 {
		t.Fatalf("close calls after first close = %d/%d/%d, want 1/1/1", first.closeCalls, second.closeCalls, third.closeCalls)
	}

	writeErr := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(writeErr, ErrSinkClosed) {
		t.Fatalf("WriteEvent() after partial close error = %v, want ErrSinkClosed", writeErr)
	}

	second.closeErr = nil
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("retry Close() error = %v, want nil", err)
	}
	if first.closeCalls != 1 || second.closeCalls != 2 || third.closeCalls != 1 {
		t.Fatalf("close calls after retry = %d/%d/%d, want 1/2/1", first.closeCalls, second.closeCalls, third.closeCalls)
	}

	if err := sink.Close(ctx); err != nil {
		t.Fatalf("idempotent Close() error = %v, want nil", err)
	}
	if first.closeCalls != 1 || second.closeCalls != 2 || third.closeCalls != 1 {
		t.Fatalf("close calls after idempotent close = %d/%d/%d, want 1/2/1", first.closeCalls, second.closeCalls, third.closeCalls)
	}
}

func TestMulti_CloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	underlying := &stubSink{}
	sink := Multi(underlying)

	if err := sink.Close(ctx); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
	if underlying.closeCalls != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", underlying.closeCalls)
	}
}

func TestMulti_CloseRejectsWrites(t *testing.T) {
	ctx := context.Background()
	sink := Multi(&stubSink{})
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	eventErr := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(eventErr, ErrSinkClosed) {
		t.Fatalf("WriteEvent() error = %v, want ErrSinkClosed", eventErr)
	}
	transcriptErr := sink.WriteTranscript(ctx, NewOutputTranscript("task-1", "session-1", "request-1", "output", 1, time.Now()))
	if !errors.Is(transcriptErr, ErrSinkClosed) {
		t.Fatalf("WriteTranscript() error = %v, want ErrSinkClosed", transcriptErr)
	}
}

func TestMulti_CanceledContextRejectsWriteAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	underlying := &stubSink{}
	sink := Multi(underlying)

	err := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteEvent() error = %v, want context.Canceled", err)
	}
	if underlying.writeEventCalls != 0 {
		t.Fatalf("underlying WriteEvent calls = %d, want 0", underlying.writeEventCalls)
	}

	err = sink.Close(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", err)
	}
	if underlying.closeCalls != 0 {
		t.Fatalf("underlying Close calls = %d, want 0 for pre-canceled context", underlying.closeCalls)
	}

	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close() after canceled attempt error = %v", err)
	}
	if underlying.closeCalls != 1 {
		t.Fatalf("underlying Close calls after active close = %d, want 1", underlying.closeCalls)
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

func TestEventSummaryTranscript_UsesStableJSONShape(t *testing.T) {
	message := "agent failed"
	record, err := NewEventSummaryTranscript(
		"task-1",
		"session-1",
		"request-1",
		EventSummary{
			Usage: UsageSummary{
				PromptTokens:     11,
				CompletionTokens: 7,
				TotalTokens:      18,
			},
			FinishReason: "stop",
			ToolCalls:    2,
			ToolResults:  1,
			Error:        &message,
			DurationMS:   123,
		},
		4,
		time.Unix(0, 4),
	)
	if err != nil {
		t.Fatalf("NewEventSummaryTranscript() error = %v", err)
	}
	if record.Kind != TranscriptKindEventSummary || record.ContentType != "application/json" {
		t.Fatalf("record kind/content type = %q/%q, want event_summary application/json", record.Kind, record.ContentType)
	}

	want := `{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18},"finish_reason":"stop","tool_calls":2,"tool_results":1,"error":"agent failed","duration_ms":123}`
	if string(record.Content) != want {
		t.Fatalf("Content = %s, want %s", record.Content, want)
	}

	record, err = NewEventSummaryTranscript("task-1", "session-1", "request-1", EventSummary{}, 5, time.Unix(0, 5))
	if err != nil {
		t.Fatalf("NewEventSummaryTranscript() zero value error = %v", err)
	}
	want = `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"finish_reason":"","tool_calls":0,"tool_results":0,"error":null,"duration_ms":0}`
	if string(record.Content) != want {
		t.Fatalf("zero-value Content = %s, want %s", record.Content, want)
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

func TestRedactionPolicy_CannotClearInputContentWithoutReplayMarker(t *testing.T) {
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
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Inputs[0].Content = ""
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_CannotReplaceInputContentWithoutRedactionMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "secret prompt", SizeBytes: 13},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Inputs[0].Content = "ok"
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_AllowsInputContentRedactionWithMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "secret prompt", SizeBytes: 13},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Inputs[0].Content = "[redacted]"
		in.Inputs[0].Redacted = true
		in.Inputs[0].RedactionTag = "secret"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if redacted.Inputs[0].Content != "[redacted]" || !redacted.Inputs[0].Redacted || redacted.Inputs[0].RedactionTag != "secret" {
		t.Fatalf("redacted input = %#v, want content redacted with marker", redacted.Inputs[0])
	}
}

func TestRedactionPolicy_CannotClearAttachmentNameWithoutReplayMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		Attachments: []AttachmentMetadata{
			{Kind: "file", Name: "secret.txt", SizeBytes: 10},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Attachments[0].Name = ""
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_CannotReplaceAttachmentNameWithoutRedactionMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		Attachments: []AttachmentMetadata{
			{Kind: "file", Name: "secret.txt", Reference: "blob://secret", SizeBytes: 10, Digest: "sha256:secret"},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Attachments[0].Name = "ok.txt"
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_AllowsAttachmentNameRedactionWithMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		Attachments: []AttachmentMetadata{
			{Kind: "file", Name: "secret.txt", Reference: "blob://secret", SizeBytes: 10, Digest: "sha256:secret"},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Attachments[0].Name = "[redacted]"
		in.Attachments[0].Redacted = true
		in.Attachments[0].RedactionTag = "secret"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if redacted.Attachments[0].Name != "[redacted]" || redacted.Attachments[0].Reference != "blob://secret" || redacted.Attachments[0].Digest != "sha256:secret" || !redacted.Attachments[0].Redacted || redacted.Attachments[0].RedactionTag != "secret" {
		t.Fatalf("redacted attachment = %#v, want name redacted with reference/digest preserved", redacted.Attachments[0])
	}
}

func TestRedactionPolicy_AllowsAttachmentMetadataRedactionWithMarker(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		Attachments: []AttachmentMetadata{
			{
				Kind:      "file",
				Name:      "secret.txt",
				Reference: "blob://secret",
				SizeBytes: 10,
				Digest:    "sha256:secret",
				Metadata:  map[string]string{"path": "/tmp/secret.txt", "owner": "alice"},
			},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Attachments[0].Name = "[redacted]"
		in.Attachments[0].Metadata["path"] = "[redacted]"
		in.Attachments[0].Redacted = true
		in.Attachments[0].RedactionTag = "secret"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if redacted.Attachments[0].Reference != "blob://secret" || redacted.Attachments[0].Digest != "sha256:secret" {
		t.Fatalf("redacted attachment = %#v, want reference/digest preserved", redacted.Attachments[0])
	}
	if redacted.Attachments[0].Metadata["path"] != "[redacted]" || redacted.Attachments[0].Metadata["owner"] != "alice" {
		t.Fatalf("redacted attachment metadata = %#v, want policy-redacted metadata", redacted.Attachments[0].Metadata)
	}
}

func TestRedactionPolicy_AllowsInputMetadataRedactionWithMarker(t *testing.T) {
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
				Content:          "[redacted]",
				ContentReference: "content://canonical",
				SizeBytes:        20,
				SourceReference:  "message://source",
				Metadata:         map[string]string{"path": "/tmp/secret.txt", "source": "user"},
				Redacted:         true,
				RedactionTag:     "secret",
			},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.Inputs[0].Metadata["path"] = "[redacted]"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if redacted.Inputs[0].ContentReference != "content://canonical" || redacted.Inputs[0].SourceReference != "message://source" {
		t.Fatalf("redacted input = %#v, want replay-critical references preserved", redacted.Inputs[0])
	}
	if redacted.Inputs[0].Metadata["path"] != "[redacted]" || redacted.Inputs[0].Metadata["source"] != "user" {
		t.Fatalf("redacted input metadata = %#v, want policy-redacted metadata", redacted.Inputs[0].Metadata)
	}
}

func TestRedactionPolicy_CannotDropRegistryContext(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:           "task-1",
		SessionID:        "session-1",
		RequestID:        "request-1",
		WorkspaceID:      "workspace-1",
		WorkspaceAlias:   "Alice",
		AgentKey:         "agent-key",
		RuntimeType:      "cli",
		RuntimeKind:      "resumable_cli",
		RegistrySnapshot: json.RawMessage(`{"partners":["Bob"]}`),
		Inputs: []RenderedInputPart{
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.RegistrySnapshot = nil
		in.RegistryReference = ""
		in.TemplateVersion = ""
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_CannotReplaceRegistrySnapshotWithoutStableReference(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:           "task-1",
		SessionID:        "session-1",
		RequestID:        "request-1",
		WorkspaceID:      "workspace-1",
		WorkspaceAlias:   "Alice",
		AgentKey:         "agent-key",
		RuntimeType:      "cli",
		RuntimeKind:      "resumable_cli",
		RegistrySnapshot: json.RawMessage(`{"partners":["Bob"]}`),
		Inputs: []RenderedInputPart{
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.RegistrySnapshot = json.RawMessage(`{}`)
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestRedactionPolicy_AllowsReplacingRegistrySnapshotWithStableReference(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:           "task-1",
		SessionID:        "session-1",
		RequestID:        "request-1",
		WorkspaceID:      "workspace-1",
		WorkspaceAlias:   "Alice",
		AgentKey:         "agent-key",
		RuntimeType:      "cli",
		RuntimeKind:      "resumable_cli",
		RegistrySnapshot: json.RawMessage(`{"partners":["Bob"]}`),
		Inputs: []RenderedInputPart{
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.RegistrySnapshot = nil
		in.RegistryReference = "registry://stable"
		in.TemplateVersion = "prompt-v2"
		return in, nil
	})

	redacted, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if len(redacted.RegistrySnapshot) != 0 || redacted.RegistryReference != "registry://stable" || redacted.TemplateVersion != "prompt-v2" {
		t.Fatalf("registry context = snapshot %s reference %q template %q, want stable replacement", redacted.RegistrySnapshot, redacted.RegistryReference, redacted.TemplateVersion)
	}
}

func TestRedactionPolicy_CannotChangeExistingStableRegistryReference(t *testing.T) {
	content := FullyRenderedRequest{
		TaskID:            "task-1",
		SessionID:         "session-1",
		RequestID:         "request-1",
		WorkspaceID:       "workspace-1",
		WorkspaceAlias:    "Alice",
		AgentKey:          "agent-key",
		RuntimeType:       "cli",
		RuntimeKind:       "resumable_cli",
		RegistryReference: "registry://stable-v1",
		TemplateVersion:   "prompt-v1",
		Inputs: []RenderedInputPart{
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		in.RegistryReference = "registry://stable-v2"
		in.TemplateVersion = "prompt-v2"
		return in, nil
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
	if redacted.Inputs[0].Metadata["source"] != "wrong" {
		t.Fatalf("redacted input metadata = %#v, want policy metadata preserved", redacted.Inputs[0].Metadata)
	}
	if redacted.Attachments[0].Kind != "file" || redacted.Attachments[0].Reference != "blob://secret" || redacted.Attachments[0].SizeBytes != 10 || redacted.Attachments[0].Digest != "sha256:secret" {
		t.Fatalf("redacted attachment structure = %#v, want original structural metadata restored", redacted.Attachments[0])
	}
	nested := redacted.PromptDiagnostics["nested"].(map[string]any)
	steps := nested["steps"].([]any)
	typed := redacted.PromptDiagnostics["typed"].([]map[string]any)
	decision := typed[0]["decision"].(map[string][]string)
	if redacted.Attachments[0].Metadata["name"] != "wrong" || redacted.PromptDiagnostics["notes_included"] != false || steps[0] != "wrong" || decision["sources"][0] != "wrong" || string(redacted.RegistrySnapshot) != `{"partners":["Mallory"]}` || redacted.RegistryReference != "registry://snapshot-1" || redacted.TemplateVersion != "prompt-v1" || redacted.Metadata["request"] != "wrong" {
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

func TestRedactionPolicy_DiagnosticsJSONLikeMutationCannotMutateOriginalRequest(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		PromptDiagnostics: map[string]any{
			"map": map[string]any{
				"items": []any{"original"},
			},
			"slice": []any{
				map[string]string{"name": "original"},
			},
			"number": json.Number("12.5"),
		},
	}
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		items := in.PromptDiagnostics["map"].(map[string]any)["items"].([]any)
		items[0] = "redacted"

		sliceEntry := in.PromptDiagnostics["slice"].([]any)[0].(map[string]string)
		sliceEntry["name"] = "redacted"
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}

	items := content.PromptDiagnostics["map"].(map[string]any)["items"].([]any)
	sliceEntry := content.PromptDiagnostics["slice"].([]any)[0].(map[string]string)
	if items[0] != "original" || sliceEntry["name"] != "original" {
		t.Fatalf("original diagnostics mutated: %#v", content.PromptDiagnostics)
	}
}

func TestRedactionPolicy_RejectsUnsupportedPromptDiagnosticsBeforePolicy(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		PromptDiagnostics: map[string]any{
			"ptr": &diagnosticValue{
				Labels: []string{"original"},
				Nested: &diagnosticNested{Value: "original"},
			},
		},
	}
	policyCalled := false
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		policyCalled = true
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
	if policyCalled {
		t.Fatal("policy was called for unsupported diagnostics")
	}
	ptr := content.PromptDiagnostics["ptr"].(*diagnosticValue)
	if ptr.Labels[0] != "original" || ptr.Nested.Value != "original" {
		t.Fatalf("original diagnostics mutated: %#v", content.PromptDiagnostics)
	}
}

func TestRedactionPolicy_RejectsUnsupportedPromptDiagnosticsShapes(t *testing.T) {
	sliceCycle := []any{nil}
	sliceCycle[0] = sliceCycle
	mapCycle := map[string]any{}
	mapCycle["self"] = mapCycle

	tests := []struct {
		name  string
		value any
	}{
		{name: "struct", value: diagnosticValue{Labels: []string{"original"}}},
		{name: "non_string_map_key", value: map[int]string{1: "original"}},
		{name: "slice_cycle", value: sliceCycle},
		{name: "map_cycle", value: mapCycle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
					{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
				},
				PromptDiagnostics: map[string]any{
					"bad": tt.value,
				},
			}
			policyCalled := false
			policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
				policyCalled = true
				return in, nil
			})

			_, err := ApplyRedaction(content, policy)
			if !errors.Is(err, ErrRedactionStructure) {
				t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
			}
			if policyCalled {
				t.Fatal("policy was called for unsupported diagnostics")
			}
		})
	}
}

func TestRedactionPolicy_RejectsNonFinitePromptDiagnosticsFloats(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "float64_nan", value: math.NaN()},
		{name: "float64_inf", value: math.Inf(1)},
		{name: "float32_inf", value: float32(math.Inf(-1))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
					{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
				},
				PromptDiagnostics: map[string]any{
					"bad": tt.value,
				},
			}
			policyCalled := false
			policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
				policyCalled = true
				return in, nil
			})

			_, err := ApplyRedaction(content, policy)
			if !errors.Is(err, ErrRedactionStructure) {
				t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
			}
			if policyCalled {
				t.Fatal("policy was called for non-finite diagnostics")
			}
		})
	}
}

func TestRedactionPolicy_RejectsInvalidJSONNumbersBeforePolicy(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "nan", value: json.Number("NaN")},
		{name: "infinity", value: json.Number("Infinity")},
		{name: "empty", value: json.Number("")},
		{name: "nested_slice_nan", value: []json.Number{json.Number("NaN")}},
		{name: "nested_map_infinity", value: map[string]json.Number{"bad": json.Number("Infinity")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
					{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
				},
				PromptDiagnostics: map[string]any{
					"bad": tt.value,
				},
			}
			policyCalled := false
			policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
				policyCalled = true
				return in, nil
			})

			_, err := ApplyRedaction(content, policy)
			if !errors.Is(err, ErrRedactionStructure) {
				t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
			}
			if policyCalled {
				t.Fatal("policy was called for invalid json.Number diagnostics")
			}
		})
	}
}

func TestApplyRedaction_NilPolicyRejectsInvalidJSONNumberDiagnostics(t *testing.T) {
	tests := []struct {
		name  string
		value json.Number
	}{
		{name: "nan", value: json.Number("NaN")},
		{name: "infinity", value: json.Number("Infinity")},
		{name: "empty", value: json.Number("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
					{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
				},
				PromptDiagnostics: map[string]any{
					"bad": tt.value,
				},
			}

			_, err := ApplyRedaction(content, nil)
			if !errors.Is(err, ErrRedactionStructure) {
				t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
			}
		})
	}
}

func TestApplyRedaction_NilPolicyRejectsNestedInvalidJSONNumberDiagnostics(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		PromptDiagnostics: map[string]any{
			"nested": map[string]any{
				"bad": []json.Number{json.Number("Infinity")},
			},
		},
	}

	_, err := ApplyRedaction(content, nil)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
}

func TestApplyRedaction_NilPolicyReturnsValidatedCloneWithValidJSONNumber(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		PromptDiagnostics: map[string]any{
			"number": json.Number("12.5"),
			"nested": map[string]any{
				"items": []any{"original"},
			},
		},
	}

	redacted, err := ApplyRedaction(content, nil)
	if err != nil {
		t.Fatalf("ApplyRedaction() error = %v", err)
	}
	if got := redacted.PromptDiagnostics["number"]; got != json.Number("12.5") {
		t.Fatalf("PromptDiagnostics[number] = %#v, want json.Number(%q)", got, "12.5")
	}

	redactedNested := redacted.PromptDiagnostics["nested"].(map[string]any)
	redactedItems := redactedNested["items"].([]any)
	redactedItems[0] = "mutated"
	redacted.PromptDiagnostics["number"] = json.Number("7")

	originalNested := content.PromptDiagnostics["nested"].(map[string]any)
	originalItems := originalNested["items"].([]any)
	if originalItems[0] != "original" || content.PromptDiagnostics["number"] != json.Number("12.5") {
		t.Fatalf("original diagnostics mutated: %#v", content.PromptDiagnostics)
	}
}

func TestRedactionPolicy_RejectsUnsupportedPromptDiagnosticsReturnedByPolicy(t *testing.T) {
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
			{Role: "user", Kind: "text", Content: "ok", SizeBytes: 2},
		},
		PromptDiagnostics: map[string]any{
			"ok": []any{"original"},
		},
	}
	policyCalled := false
	policy := RedactionPolicyFunc(func(in FullyRenderedRequest) (FullyRenderedRequest, error) {
		policyCalled = true
		in.PromptDiagnostics["bad"] = func() {}
		return in, nil
	})

	_, err := ApplyRedaction(content, policy)
	if !errors.Is(err, ErrRedactionStructure) {
		t.Fatalf("ApplyRedaction() error = %v, want ErrRedactionStructure", err)
	}
	if !policyCalled {
		t.Fatal("policy was not called")
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

func TestMemorySink_CloseRejectsTranscriptWrites(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	err := sink.WriteTranscript(ctx, NewOutputTranscript("task-1", "session-1", "request-1", "output", 1, time.Now()))
	if !errors.Is(err, ErrSinkClosed) {
		t.Fatalf("WriteTranscript() error = %v, want ErrSinkClosed", err)
	}
}

func TestMemorySink_CanceledContextRejectsWriteAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := NewMemorySink()

	eventErr := sink.WriteEvent(ctx, testEvent("task-1", EventTypeTaskCreated, 1))
	if !errors.Is(eventErr, context.Canceled) {
		t.Fatalf("WriteEvent() error = %v, want context.Canceled", eventErr)
	}
	transcriptErr := sink.WriteTranscript(ctx, NewOutputTranscript("task-1", "session-1", "request-1", "output", 1, time.Now()))
	if !errors.Is(transcriptErr, context.Canceled) {
		t.Fatalf("WriteTranscript() error = %v, want context.Canceled", transcriptErr)
	}
	closeErr := sink.Close(ctx)
	if !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", closeErr)
	}
}

func TestMemorySink_AccessorsReturnIsolatedRecordContent(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	event := testEvent("task-1", EventTypeTaskCreated, 1)
	transcript := NewOutputTranscript("task-1", "session-1", "request-1", "output", 1, time.Now())

	if err := sink.WriteEvent(ctx, event); err != nil {
		t.Fatalf("WriteEvent() error = %v", err)
	}
	if err := sink.WriteTranscript(ctx, transcript); err != nil {
		t.Fatalf("WriteTranscript() error = %v", err)
	}

	event.Payload[0] = '['
	transcript.Content[0] = 'X'

	events := sink.Events("task-1")
	transcripts := sink.Transcripts("task-1")
	events[0].Payload[0] = '['
	transcripts[0].Content[0] = 'X'

	events = sink.Events("task-1")
	transcripts = sink.Transcripts("task-1")
	if string(events[0].Payload) != `{"ok":true}` {
		t.Fatalf("stored event payload = %s, want original JSON", events[0].Payload)
	}
	if string(transcripts[0].Content) != "output" {
		t.Fatalf("stored transcript content = %q, want output", transcripts[0].Content)
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
	closeCalls           int
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
	s.closeCalls++
	if err := ctx.Err(); err != nil && s.closeErr == nil {
		return err
	}
	return s.closeErr
}

type blockingFanoutSink struct {
	mu                sync.Mutex
	firstCallStarted  chan struct{}
	secondCallStarted chan struct{}
	releaseFirst      chan struct{}
	calls             int
}

func newBlockingFanoutSink() *blockingFanoutSink {
	return &blockingFanoutSink{
		firstCallStarted:  make(chan struct{}),
		secondCallStarted: make(chan struct{}),
		releaseFirst:      make(chan struct{}),
	}
}

func (s *blockingFanoutSink) WriteEvent(ctx context.Context, record EventRecord) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	if call == 1 {
		close(s.firstCallStarted)
	}
	if call == 2 {
		close(s.secondCallStarted)
	}
	s.mu.Unlock()

	if call == 1 {
		select {
		case <-s.releaseFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *blockingFanoutSink) WriteTranscript(ctx context.Context, record TranscriptRecord) error {
	return nil
}

func (s *blockingFanoutSink) Close(ctx context.Context) error {
	return nil
}

func (s *blockingFanoutSink) waitForFirstCall() error {
	select {
	case <-s.firstCallStarted:
		return nil
	case <-time.After(time.Second):
		return errors.New("timed out waiting for first write to reach first sink")
	}
}

func (s *blockingFanoutSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *blockingFanoutSink) releaseFirstCall() {
	close(s.releaseFirst)
}

type recordingSink struct {
	mu     sync.Mutex
	events []EventRecord
}

func (s *recordingSink) WriteEvent(ctx context.Context, record EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, cloneEventRecord(record))
	return nil
}

func (s *recordingSink) WriteTranscript(ctx context.Context, record TranscriptRecord) error {
	return nil
}

func (s *recordingSink) Close(ctx context.Context) error {
	return nil
}

func (s *recordingSink) eventSeqs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	seqs := make([]uint64, len(s.events))
	for i, event := range s.events {
		seqs[i] = event.Seq
	}
	return seqs
}

type diagnosticValue struct {
	Labels []string
	Nested *diagnosticNested
}

type diagnosticNested struct {
	Value string
}
