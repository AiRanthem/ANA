// Package audit defines the orchestrator's synchronous, fail-fast audit sink.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/AiRanthem/ANA/pkg/orchestrator/idgen"
)

const SchemaV1 = "v1"

var (
	ErrSinkClosed         = errors.New("audit sink closed")
	ErrSinkBackpressure   = errors.New("audit sink backpressure")
	ErrNoSink             = errors.New("audit sink missing")
	ErrRedactionStructure = errors.New("audit redaction removed structural metadata")
)

// EventType enumerates the audit event taxonomy. The values MUST stay in
// lockstep with events.EventType in pkg/orchestrator/events/events.go —
// AGENTS.md forbids cross-imports between audit and events, so updates here
// require a matching edit in events.go.
type EventType string

const (
	EventTypeTaskCreated      EventType = "task.created"
	EventTypeTaskRunning      EventType = "task.running"
	EventTypeTaskCompleted    EventType = "task.completed"
	EventTypeTaskFailed       EventType = "task.failed"
	EventTypeTaskCancelled    EventType = "task.cancelled"
	EventTypeSessionOpened    EventType = "session.opened"
	EventTypeSessionPaused    EventType = "session.paused"
	EventTypeSessionResumed   EventType = "session.resumed"
	EventTypeSessionClosed    EventType = "session.closed"
	EventTypeSessionFailed    EventType = "session.failed"
	EventTypeRequestCreated   EventType = "request.created"
	EventTypeRequestRunning   EventType = "request.running"
	EventTypeRequestCompleted EventType = "request.completed"
	EventTypeRequestFailed    EventType = "request.failed"
	EventTypeRequestTextChunk EventType = "request.text_chunk"
	EventTypeRouteDirective   EventType = "route.directive"
)

type TranscriptKind string

const (
	TranscriptKindInput        TranscriptKind = "input"
	TranscriptKindOutput       TranscriptKind = "output"
	TranscriptKindEventSummary TranscriptKind = "event_summary"
)

// Sink is the durable audit boundary. Write calls are synchronous: a nil error
// means the sink accepted responsibility for the record.
type Sink interface {
	WriteEvent(ctx context.Context, record EventRecord) error
	WriteTranscript(ctx context.Context, record TranscriptRecord) error
	Close(ctx context.Context) error
}

type EventRecord struct {
	EventID    string
	TaskID     idgen.TaskID
	SessionID  idgen.SessionID
	RequestID  idgen.RequestID
	Type       EventType
	Seq        uint64
	OccurredAt time.Time
	Payload    []byte
	Schema     string
}

type TranscriptRecord struct {
	TaskID      idgen.TaskID
	SessionID   idgen.SessionID
	RequestID   idgen.RequestID
	Kind        TranscriptKind
	Content     []byte
	ContentType string
	Seq         uint64
	Schema      string
	CreatedAt   time.Time
}

type FullyRenderedRequest struct {
	TaskID            idgen.TaskID         `json:"task_id"`
	SessionID         idgen.SessionID      `json:"session_id"`
	RequestID         idgen.RequestID      `json:"request_id"`
	WorkspaceID       string               `json:"workspace_id"`
	WorkspaceAlias    string               `json:"workspace_alias"`
	AgentKey          string               `json:"agent_key"`
	RuntimeType       string               `json:"runtime_type"`
	RuntimeKind       string               `json:"runtime_kind"`
	Inputs            []RenderedInputPart  `json:"inputs"`
	Attachments       []AttachmentMetadata `json:"attachments,omitempty"`
	PromptDiagnostics map[string]any       `json:"prompt_diagnostics,omitempty"`
	RegistrySnapshot  json.RawMessage      `json:"registry_snapshot,omitempty"`
	RegistryReference string               `json:"registry_reference,omitempty"`
	TemplateVersion   string               `json:"template_version,omitempty"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
}

type RenderedInputPart struct {
	Role             string            `json:"role"`
	Kind             string            `json:"kind"`
	Content          string            `json:"content,omitempty"`
	ContentReference string            `json:"content_reference,omitempty"`
	SizeBytes        int               `json:"size_bytes"`
	SourceReference  string            `json:"source_reference,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Redacted         bool              `json:"redacted,omitempty"`
	RedactionTag     string            `json:"redaction_tag,omitempty"`
}

type AttachmentMetadata struct {
	Kind         string            `json:"kind"`
	Name         string            `json:"name,omitempty"`
	Reference    string            `json:"reference,omitempty"`
	SizeBytes    int64             `json:"size_bytes,omitempty"`
	Digest       string            `json:"digest,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Redacted     bool              `json:"redacted,omitempty"`
	RedactionTag string            `json:"redaction_tag,omitempty"`
}

type RedactionPolicy interface {
	RedactRequest(FullyRenderedRequest) (FullyRenderedRequest, error)
}

type RedactionPolicyFunc func(FullyRenderedRequest) (FullyRenderedRequest, error)

func (f RedactionPolicyFunc) RedactRequest(request FullyRenderedRequest) (FullyRenderedRequest, error) {
	return f(request)
}

func ApplyRedaction(request FullyRenderedRequest, policy RedactionPolicy) (FullyRenderedRequest, error) {
	if policy == nil {
		return request, nil
	}
	original := cloneFullyRenderedRequest(request)
	redacted, err := policy.RedactRequest(cloneFullyRenderedRequest(request))
	if err != nil {
		return FullyRenderedRequest{}, fmt.Errorf("redact audit request: %w", err)
	}
	restored, err := preserveRequiredMetadata(original, redacted)
	if err != nil {
		return FullyRenderedRequest{}, err
	}
	return restored, nil
}

func NewInputTranscript(request FullyRenderedRequest, seq uint64, createdAt time.Time) (TranscriptRecord, error) {
	content, err := json.Marshal(request)
	if err != nil {
		return TranscriptRecord{}, fmt.Errorf("marshal input transcript: %w", err)
	}
	return TranscriptRecord{
		TaskID:      request.TaskID,
		SessionID:   request.SessionID,
		RequestID:   request.RequestID,
		Kind:        TranscriptKindInput,
		Content:     content,
		ContentType: "application/json",
		Seq:         seq,
		Schema:      SchemaV1,
		CreatedAt:   createdAt,
	}, nil
}

func NewOutputTranscript(taskID idgen.TaskID, sessionID idgen.SessionID, requestID idgen.RequestID, output string, seq uint64, createdAt time.Time) TranscriptRecord {
	return TranscriptRecord{
		TaskID:      taskID,
		SessionID:   sessionID,
		RequestID:   requestID,
		Kind:        TranscriptKindOutput,
		Content:     []byte(output),
		ContentType: "text/plain; charset=utf-8",
		Seq:         seq,
		Schema:      SchemaV1,
		CreatedAt:   createdAt,
	}
}

func NewEventSummaryTranscript(taskID idgen.TaskID, sessionID idgen.SessionID, requestID idgen.RequestID, summary any, seq uint64, createdAt time.Time) (TranscriptRecord, error) {
	content, err := json.Marshal(summary)
	if err != nil {
		return TranscriptRecord{}, fmt.Errorf("marshal event summary transcript: %w", err)
	}
	return TranscriptRecord{
		TaskID:      taskID,
		SessionID:   sessionID,
		RequestID:   requestID,
		Kind:        TranscriptKindEventSummary,
		Content:     content,
		ContentType: "application/json",
		Seq:         seq,
		Schema:      SchemaV1,
		CreatedAt:   createdAt,
	}, nil
}

type MemorySink struct {
	mu          sync.Mutex
	closed      bool
	events      []EventRecord
	transcripts []TranscriptRecord
}

func NewMemorySink() *MemorySink {
	return &MemorySink{}
}

func (s *MemorySink) WriteEvent(ctx context.Context, record EventRecord) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write audit event %q task %q: %w", record.EventID, record.TaskID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("write audit event %q task %q: %w", record.EventID, record.TaskID, ErrSinkClosed)
	}
	s.events = append(s.events, cloneEventRecord(record))
	return nil
}

func (s *MemorySink) WriteTranscript(ctx context.Context, record TranscriptRecord) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write audit transcript task %q session %q request %q kind %q seq %d: %w", record.TaskID, record.SessionID, record.RequestID, record.Kind, record.Seq, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("write audit transcript task %q session %q request %q kind %q seq %d: %w", record.TaskID, record.SessionID, record.RequestID, record.Kind, record.Seq, ErrSinkClosed)
	}
	s.transcripts = append(s.transcripts, cloneTranscriptRecord(record))
	return nil
}

func (s *MemorySink) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("close audit memory sink: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *MemorySink) Events(taskID idgen.TaskID) []EventRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]EventRecord, 0, len(s.events))
	for _, record := range s.events {
		if record.TaskID == taskID {
			records = append(records, cloneEventRecord(record))
		}
	}
	return records
}

func (s *MemorySink) Transcripts(taskID idgen.TaskID) []TranscriptRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]TranscriptRecord, 0, len(s.transcripts))
	for _, record := range s.transcripts {
		if record.TaskID == taskID {
			records = append(records, cloneTranscriptRecord(record))
		}
	}
	return records
}

func (s *MemorySink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
	s.transcripts = nil
	s.closed = false
}

func Multi(sinks ...Sink) Sink {
	filtered := make([]Sink, 0, len(sinks))
	for _, sink := range sinks {
		if sink != nil {
			filtered = append(filtered, sink)
		}
	}
	return multiSink{sinks: filtered}
}

type multiSink struct {
	sinks []Sink
}

func (s multiSink) WriteEvent(ctx context.Context, record EventRecord) error {
	if len(s.sinks) == 0 {
		return fmt.Errorf("write audit event task %q event %q: %w", record.TaskID, record.EventID, ErrNoSink)
	}
	for i, sink := range s.sinks {
		if err := sink.WriteEvent(ctx, record); err != nil {
			return fmt.Errorf("write audit event sink %d task %q event %q: %w", i, record.TaskID, record.EventID, err)
		}
	}
	return nil
}

func (s multiSink) WriteTranscript(ctx context.Context, record TranscriptRecord) error {
	if len(s.sinks) == 0 {
		return fmt.Errorf("write audit transcript task %q session %q request %q kind %q seq %d: %w", record.TaskID, record.SessionID, record.RequestID, record.Kind, record.Seq, ErrNoSink)
	}
	for i, sink := range s.sinks {
		if err := sink.WriteTranscript(ctx, record); err != nil {
			return fmt.Errorf("write audit transcript sink %d task %q session %q request %q kind %q seq %d: %w", i, record.TaskID, record.SessionID, record.RequestID, record.Kind, record.Seq, err)
		}
	}
	return nil
}

func (s multiSink) Close(ctx context.Context) error {
	for i, sink := range s.sinks {
		if err := sink.Close(ctx); err != nil {
			return fmt.Errorf("close audit sink %d: %w", i, err)
		}
	}
	return nil
}

func preserveRequiredMetadata(original FullyRenderedRequest, redacted FullyRenderedRequest) (FullyRenderedRequest, error) {
	if len(redacted.Inputs) != len(original.Inputs) {
		return FullyRenderedRequest{}, fmt.Errorf("redact audit request inputs length %d want %d: %w", len(redacted.Inputs), len(original.Inputs), ErrRedactionStructure)
	}
	if len(redacted.Attachments) != len(original.Attachments) {
		return FullyRenderedRequest{}, fmt.Errorf("redact audit request attachments length %d want %d: %w", len(redacted.Attachments), len(original.Attachments), ErrRedactionStructure)
	}
	redacted.TaskID = original.TaskID
	redacted.SessionID = original.SessionID
	redacted.RequestID = original.RequestID
	redacted.WorkspaceID = original.WorkspaceID
	redacted.WorkspaceAlias = original.WorkspaceAlias
	redacted.AgentKey = original.AgentKey
	redacted.RuntimeType = original.RuntimeType
	redacted.RuntimeKind = original.RuntimeKind
	redacted.PromptDiagnostics = cloneAnyMap(redacted.PromptDiagnostics)
	redacted.RegistrySnapshot = append(json.RawMessage(nil), redacted.RegistrySnapshot...)
	redacted.RegistryReference = original.RegistryReference
	redacted.TemplateVersion = original.TemplateVersion
	redacted.Metadata = cloneStringMap(redacted.Metadata)
	for i := range redacted.Inputs {
		redacted.Inputs[i].Role = original.Inputs[i].Role
		redacted.Inputs[i].Kind = original.Inputs[i].Kind
		redacted.Inputs[i].SizeBytes = original.Inputs[i].SizeBytes
		redacted.Inputs[i].SourceReference = original.Inputs[i].SourceReference
		redacted.Inputs[i].ContentReference = original.Inputs[i].ContentReference
		redacted.Inputs[i].Metadata = cloneStringMap(original.Inputs[i].Metadata)
	}
	for i := range redacted.Attachments {
		redacted.Attachments[i].Kind = original.Attachments[i].Kind
		redacted.Attachments[i].Reference = original.Attachments[i].Reference
		redacted.Attachments[i].SizeBytes = original.Attachments[i].SizeBytes
		redacted.Attachments[i].Digest = original.Attachments[i].Digest
		redacted.Attachments[i].Metadata = cloneStringMap(original.Attachments[i].Metadata)
	}
	return redacted, nil
}

func cloneFullyRenderedRequest(request FullyRenderedRequest) FullyRenderedRequest {
	clone := request
	clone.Inputs = make([]RenderedInputPart, len(request.Inputs))
	for i, part := range request.Inputs {
		clone.Inputs[i] = part
		clone.Inputs[i].Metadata = cloneStringMap(part.Metadata)
	}
	clone.Attachments = make([]AttachmentMetadata, len(request.Attachments))
	for i, attachment := range request.Attachments {
		clone.Attachments[i] = attachment
		clone.Attachments[i].Metadata = cloneStringMap(attachment.Metadata)
	}
	clone.PromptDiagnostics = cloneAnyMap(request.PromptDiagnostics)
	clone.RegistrySnapshot = append(json.RawMessage(nil), request.RegistrySnapshot...)
	clone.Metadata = cloneStringMap(request.Metadata)
	return clone
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneAnyValue(value)
	}
	return out
}

func cloneAnyValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneReflectValue(reflect.ValueOf(value)).Interface()
}

func cloneReflectValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneReflectValue(value.Elem())
		wrapped := reflect.New(value.Type()).Elem()
		wrapped.Set(cloned)
		return wrapped
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			cloned.SetMapIndex(cloneReflectValue(iter.Key()), cloneReflectValue(iter.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	default:
		return value
	}
}

func cloneEventRecord(record EventRecord) EventRecord {
	clone := record
	clone.Payload = append([]byte(nil), record.Payload...)
	return clone
}

func cloneTranscriptRecord(record TranscriptRecord) TranscriptRecord {
	clone := record
	clone.Content = append([]byte(nil), record.Content...)
	return clone
}
