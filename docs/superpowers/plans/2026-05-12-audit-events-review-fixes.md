# Audit / Events Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the 9 issues identified by the code review of `pkg/orchestrator/audit` and `pkg/orchestrator/events`: align DESIGN/PLAN documents with the implementation, unify the `Seq` field type across both packages, document leaked API surface, fix one tautological test, harden one flaky test, fix the multi-sink Close semantics, and add the missing ordering tests.

**Architecture:** All changes are either (a) doc edits to `DESIGN.md` / `PLAN.md`, (b) localized fixes inside the two leaf packages `pkg/orchestrator/audit` and `pkg/orchestrator/events`, or (c) test additions/edits in the same two packages. No new dependencies, no public-interface breakage beyond a single integer-width change (`int` → `uint64`) on a field that has no in-tree callers yet.

**Tech Stack:** Go (stdlib only), `github.com/AiRanthem/ANA/pkg/orchestrator/idgen` for ID types.

---

## File Map

- Modify: `pkg/orchestrator/DESIGN.md` (§8.2 Event schema — add `Seq`)
- Modify: `pkg/orchestrator/audit/PLAN.md` (public surface — declare `ErrNoSink`; record table — add `Seq`)
- Modify: `pkg/orchestrator/events/PLAN.md` (public surface — declare `Dropped()`; Drop policy — describe inline coalescing; `Publish` semantics on empty TaskID)
- Modify: `pkg/orchestrator/audit/audit.go` (change `EventRecord.Seq` from `int` to `uint64`; add EventType sync comment; fix `multiSink.Close` zero-sink behavior)
- Modify: `pkg/orchestrator/audit/audit_test.go` (update Seq types in test helpers; fix `TestFailingSinkReturnsErrorToCaller` to go through `Multi`; add transcript ordering test)
- Modify: `pkg/orchestrator/events/events.go` (add EventType sync comment)
- Modify: `pkg/orchestrator/events/events_test.go` (harden `TestBus_ConcurrentPublishSubscribe` buffer; add multi-event ordering test)

---

## Task 1: Document the `Seq` field in DESIGN.md §8.2

**Files:**
- Modify: `pkg/orchestrator/DESIGN.md:723-733`

Background: The `Event` struct in code carries a `Seq` field used for per-task monotonic ordering, but DESIGN.md §8.2 omits it. AGENTS.md requires DESIGN updates for event-taxonomy changes.

- [ ] **Step 1: Update the Event struct in DESIGN.md**

Replace lines 723-733 of `pkg/orchestrator/DESIGN.md`:

```go
type Event struct {
    EventID    string
    TaskID     TaskID
    SessionID  SessionID  // empty when type == task.*
    RequestID  RequestID  // empty when type == task.* / session.*
    Type       EventType
    Seq        uint64     // monotonic per (task, session, request); audit and bus use the same width
    OccurredAt time.Time
    Payload    EventPayload // typed per Type
}
```

- [ ] **Step 2: Verify the file still reads coherently**

Run: `grep -n "Seq" pkg/orchestrator/DESIGN.md`
Expected: at least one match showing `Seq` inside the Event struct block; surrounding prose (`The full type list…`) unchanged.

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/DESIGN.md
git commit -m "docs(orchestrator): add Seq to Event schema in DESIGN §8.2"
```

---

## Task 2: Unify `Seq` type to `uint64` in `audit.EventRecord`

**Files:**
- Modify: `pkg/orchestrator/audit/audit.go:62-72`
- Modify: `pkg/orchestrator/audit/audit_test.go:405-417` (test helper `testEvent`)

Background: `events.Event.Seq` is `uint64`, `audit.EventRecord.Seq` is `int`. Same logical field, different widths — any future bridge code will need explicit conversion and lose negative-value safety. Pick `uint64` to match the bus.

- [ ] **Step 1: Change the EventRecord struct in audit.go**

In `pkg/orchestrator/audit/audit.go`, change the `Seq` field type:

```go
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
```

- [ ] **Step 2: Adjust the `TranscriptRecord.Seq` field for consistency**

Same file, `TranscriptRecord.Seq` is also `int`. Change to `uint64`:

```go
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
```

- [ ] **Step 3: Adjust the `NewInputTranscript` / `NewOutputTranscript` / `NewEventSummaryTranscript` signatures**

In the same file, change each constructor's `seq int` parameter to `seq uint64`:

```go
func NewInputTranscript(request FullyRenderedRequest, seq uint64, createdAt time.Time) (TranscriptRecord, error) {
    // body unchanged
}

func NewOutputTranscript(taskID idgen.TaskID, sessionID idgen.SessionID, requestID idgen.RequestID, output string, seq uint64, createdAt time.Time) TranscriptRecord {
    // body unchanged
}

func NewEventSummaryTranscript(taskID idgen.TaskID, sessionID idgen.SessionID, requestID idgen.RequestID, summary any, seq uint64, createdAt time.Time) (TranscriptRecord, error) {
    // body unchanged
}
```

- [ ] **Step 4: Adjust error format strings that print `Seq`**

The two `WriteTranscript` error messages and the two `multiSink.WriteTranscript` wrappers use `%d` for `Seq`. `%d` is correct for `uint64` too, so no change needed — verify:

Run: `grep -n "kind %q seq %d" pkg/orchestrator/audit/audit.go`
Expected: 4 lines, all unchanged.

- [ ] **Step 5: Update the `testEvent` helper in audit_test.go**

In `pkg/orchestrator/audit/audit_test.go`, change the `seq int` parameter to `seq uint64`:

```go
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
```

- [ ] **Step 6: Update `TestMemorySink_ConcurrentWritesAreSafe` call sites**

Same file, in `TestMemorySink_ConcurrentWritesAreSafe` (lines 364-390), the inner loop builds an `int` and passes it to `testEvent`. Update both inner expressions:

```go
for seq := range writesPerWriter {
    record := testEvent(taskID, EventTypeRequestTextChunk, uint64(writer*writesPerWriter+seq))
    if err := sink.WriteEvent(ctx, record); err != nil {
        t.Errorf("WriteEvent() error = %v", err)
    }
}
```

- [ ] **Step 7: Update `TestFailingSinkReturnsErrorToCaller` literal `Seq: 1`**

Same file, the literal `Seq: 1` at line ~99 is implicitly typed — no source change required, but verify the file compiles.

- [ ] **Step 8: Run audit tests**

Run: `go test ./pkg/orchestrator/audit/... -race`
Expected: all tests pass; no `int` / `uint64` mismatch errors.

- [ ] **Step 9: Commit**

```bash
git add pkg/orchestrator/audit/audit.go pkg/orchestrator/audit/audit_test.go
git commit -m "refactor(audit): unify Seq field type to uint64 across audit and events"
```

---

## Task 3: Add `Seq` to audit/PLAN.md record-shape tables

**Files:**
- Modify: `pkg/orchestrator/audit/PLAN.md:70-79`

- [ ] **Step 1: Add Seq row to the EventRecord table**

In `pkg/orchestrator/audit/PLAN.md`, the EventRecord table currently has 8 rows. Insert a `Seq` row after `Type`:

```markdown
| Field        | Type        | Notes |
|--------------|-------------|-------|
| `EventID`    | `string`    | Globally unique; generated by `idgen` |
| `TaskID`     | `TaskID`    |       |
| `SessionID`  | `SessionID` | Empty for `task.*` events |
| `RequestID`  | `RequestID` | Empty for `task.*` and `session.*` events |
| `Type`       | `EventType` | Mirrors `events.Event.Type` (see `DESIGN.md` §8.1) |
| `Seq`        | `uint64`    | Monotonic per (task, session, request); mirrors `events.Event.Seq` |
| `OccurredAt` | `time.Time` |       |
| `Payload`    | `[]byte`    | JSON serialization of the typed payload (see `events/PLAN.md`) |
| `Schema`     | `string`    | Schema version label (`v1`) for forward compat |
```

- [ ] **Step 2: Adjust the TranscriptRecord description**

Same file, the `TranscriptRecord` section references DESIGN.md §9. The DESIGN doc lists `Seq` as `int` — update DESIGN.md too:

In `pkg/orchestrator/DESIGN.md` §9 transcript table (around line 760-770), change the `Seq` row's Type column from `int` to `uint64`.

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/audit/PLAN.md pkg/orchestrator/DESIGN.md
git commit -m "docs(audit): record Seq field in PLAN and DESIGN transcript tables"
```

---

## Task 4: Declare `ErrNoSink` in audit/PLAN.md public surface

**Files:**
- Modify: `pkg/orchestrator/audit/PLAN.md:29`

- [ ] **Step 1: Add ErrNoSink to the sentinel list**

In `pkg/orchestrator/audit/PLAN.md`, find the line:

```markdown
- Sentinels: `ErrSinkClosed`, `ErrSinkBackpressure`.
```

Replace with:

```markdown
- Sentinels: `ErrSinkClosed`, `ErrSinkBackpressure`, `ErrNoSink` (returned by `Multi(...)` write paths when constructed with zero non-nil sinks), `ErrRedactionStructure` (returned by `ApplyRedaction` when the policy drops structural metadata).
```

- [ ] **Step 2: Commit**

```bash
git add pkg/orchestrator/audit/PLAN.md
git commit -m "docs(audit): declare ErrNoSink and ErrRedactionStructure in PLAN public surface"
```

---

## Task 5: Declare `Subscription.Dropped()` in events/PLAN.md public surface

**Files:**
- Modify: `pkg/orchestrator/events/PLAN.md:17-20`

- [ ] **Step 1: Add Dropped() method to the Subscription interface listing**

In `pkg/orchestrator/events/PLAN.md`, replace the Subscription block:

```markdown
- `Subscription` interface:
  - `Events() <-chan Event`
  - `Errors() <-chan error` — surface non-fatal subscriber issues
  - `Dropped() uint64` — cumulative count of events dropped for this subscriber
  - `Close() error`
```

- [ ] **Step 2: Commit**

```bash
git add pkg/orchestrator/events/PLAN.md
git commit -m "docs(events): declare Subscription.Dropped() in PLAN public surface"
```

---

## Task 6: Align events/PLAN.md drop-policy and `Publish` semantics with the implementation

**Files:**
- Modify: `pkg/orchestrator/events/PLAN.md:66-72` (Drop policy)
- Modify: `pkg/orchestrator/events/PLAN.md:87-89` (Edge cases — empty TaskID)

Background: PLAN says drops are flushed "periodically (e.g., every 1 second per subscriber)" but the implementation pushes a `SubscriberLaggedError` inline on every drop, coalescing to the latest count when `Errors()` is full. PLAN also says `Publish` with empty TaskID "rejects" — implementation silently returns `(0, 0)` because the interface has no error return.

- [ ] **Step 1: Rewrite the Drop policy section**

Replace lines 66-72 of `pkg/orchestrator/events/PLAN.md`:

```markdown
## Drop policy

- v1 uses pure non-blocking buffered channels per subscriber. No coalescing
  of chunk events.
- A drop produces a `SubscriberLaggedError` pushed inline onto the
  subscriber's `Errors()` channel. When that channel (buffer size 1) is
  full, the bus drops the older notification and replaces it with the
  newer one — consumers always see the latest cumulative `Dropped()`
  count, never a stale one. They may miss intermediate counts.
- The bus does not implement backoff or rate limiting in v1.
```

- [ ] **Step 2: Soften the empty-TaskID rejection language**

Replace lines 87-89 of the same file:

```markdown
- Publishing with `task_id == ""`: the bus ignores the publish (zero
  delivered, zero dropped). The `Bus.Publish` signature has no error
  return because publishers are best-effort; callers MUST validate
  `task_id` before invoking. Same for an already-cancelled `ctx`.
```

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/events/PLAN.md
git commit -m "docs(events): align PLAN drop policy and Publish semantics with implementation"
```

---

## Task 7: Add cross-package sync comment to `EventType` constants

**Files:**
- Modify: `pkg/orchestrator/audit/audit.go:25-44`
- Modify: `pkg/orchestrator/events/events.go:21-40`

Background: The two packages cannot import each other (per AGENTS.md), so the `EventType` constants are duplicated. Both blocks need a one-line comment marker reminding maintainers to keep them in sync.

- [ ] **Step 1: Add the sync comment in audit.go**

In `pkg/orchestrator/audit/audit.go`, replace the `EventType` declaration with:

```go
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
```

- [ ] **Step 2: Add the matching sync comment in events.go**

In `pkg/orchestrator/events/events.go`, replace the `EventType` declaration with:

```go
// EventType enumerates the runtime bus event taxonomy. The values MUST stay
// in lockstep with audit.EventType in pkg/orchestrator/audit/audit.go —
// AGENTS.md forbids cross-imports between events and audit, so updates here
// require a matching edit in audit.go.
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
```

- [ ] **Step 3: Run vet to confirm no formatting drift**

Run: `gofmt -s -d pkg/orchestrator/audit/audit.go pkg/orchestrator/events/events.go && go vet ./pkg/orchestrator/...`
Expected: zero output from `gofmt -d`; vet clean.

- [ ] **Step 4: Commit**

```bash
git add pkg/orchestrator/audit/audit.go pkg/orchestrator/events/events.go
git commit -m "docs(audit,events): annotate cross-package EventType duplication"
```

---

## Task 8: Fix `multiSink.Close` to return nil when no sinks are configured

**Files:**
- Modify: `pkg/orchestrator/audit/audit.go:320-330`
- Modify: `pkg/orchestrator/audit/audit_test.go` (new test)

Background: `WriteEvent` / `WriteTranscript` returning `ErrNoSink` on empty sinks is correct — silently dropping audit data would be unsafe. `Close` has no data-loss risk; closing a sink with nothing to close should succeed.

- [ ] **Step 1: Write the failing test**

Append to `pkg/orchestrator/audit/audit_test.go`:

```go
func TestMulti_CloseWithNoSinksIsNoop(t *testing.T) {
    ctx := context.Background()
    sink := Multi(nil)

    if err := sink.Close(ctx); err != nil {
        t.Fatalf("Close() error = %v, want nil for empty Multi", err)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/orchestrator/audit -run TestMulti_CloseWithNoSinksIsNoop -v`
Expected: FAIL with `Close() error = close audit multi sink: audit sink missing, want nil for empty Multi`.

- [ ] **Step 3: Fix `multiSink.Close`**

In `pkg/orchestrator/audit/audit.go`, replace lines 320-330:

```go
func (s multiSink) Close(ctx context.Context) error {
    for i, sink := range s.sinks {
        if err := sink.Close(ctx); err != nil {
            return fmt.Errorf("close audit sink %d: %w", i, err)
        }
    }
    return nil
}
```

(Removed the `len(s.sinks) == 0` early-error path; the empty range is a clean no-op.)

- [ ] **Step 4: Run the new test to verify it passes**

Run: `go test ./pkg/orchestrator/audit -run TestMulti_CloseWithNoSinksIsNoop -v`
Expected: PASS.

- [ ] **Step 5: Run full audit tests to confirm no regression**

Run: `go test ./pkg/orchestrator/audit/... -race`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/orchestrator/audit/audit.go pkg/orchestrator/audit/audit_test.go
git commit -m "fix(audit): multiSink.Close is a no-op when no sinks are configured"
```

---

## Task 9: Replace tautological `TestFailingSinkReturnsErrorToCaller` with a real Multi fail-fast test

**Files:**
- Modify: `pkg/orchestrator/audit/audit_test.go:88-108`

Background: The current test calls `stubSink.WriteTranscript` directly and asserts the stub returns the error it was configured with. It does not exercise any package code. The intended coverage — Multi propagating the first failing sink's error — is missing.

- [ ] **Step 1: Extend `stubSink` with a transcript call counter**

The existing stub only tracks `writeEventCalls`. The new test needs the transcript counterpart so it can assert short-circuiting precisely. In `pkg/orchestrator/audit/audit_test.go`, replace the `stubSink` block (lines 419-437):

```go
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
```

- [ ] **Step 2: Replace the tautological test with a real Multi pipeline assertion**

In the same file, delete the entire `TestFailingSinkReturnsErrorToCaller` function (lines 88-108) and add in its place:

```go
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
```

- [ ] **Step 3: Run the new test**

Run: `go test ./pkg/orchestrator/audit -run TestMulti_PropagatesFirstFailingTranscriptError -v`
Expected: PASS.

- [ ] **Step 4: Run full audit tests**

Run: `go test ./pkg/orchestrator/audit/... -race`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/orchestrator/audit/audit_test.go
git commit -m "test(audit): exercise Multi fail-fast through real pipeline, not stub directly"
```

---

## Task 10: Add audit transcript ordering test

**Files:**
- Modify: `pkg/orchestrator/audit/audit_test.go` (new test)

Background: `audit/PLAN.md` test 1 says "write events + transcripts, accessors return them in insertion order per task." The current `TestMemorySink_PreservesPerTaskOrder` only writes events.

- [ ] **Step 1: Write the failing test**

Append to `pkg/orchestrator/audit/audit_test.go`:

```go
func TestMemorySink_PreservesTranscriptInsertionOrder(t *testing.T) {
    ctx := context.Background()
    sink := NewMemorySink()
    taskID := idgen.TaskID("task-ordering")

    transcripts := []TranscriptRecord{
        NewOutputTranscript(taskID, "session-1", "request-1", "first", 1, time.Unix(0, 1)),
        NewOutputTranscript(taskID, "session-1", "request-1", "second", 2, time.Unix(0, 2)),
        NewOutputTranscript(taskID, "session-1", "request-2", "third", 1, time.Unix(0, 3)),
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
```

- [ ] **Step 2: Run the test**

Run: `go test ./pkg/orchestrator/audit -run TestMemorySink_PreservesTranscriptInsertionOrder -v`
Expected: PASS (the implementation already preserves order; this just locks it in).

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/audit/audit_test.go
git commit -m "test(audit): assert MemorySink transcript insertion order per task"
```

---

## Task 11: Add events multi-event ordering test

**Files:**
- Modify: `pkg/orchestrator/events/events_test.go` (new test)

Background: `events/PLAN.md` test 1 says "Subscribe → Publish → Events delivered in order." The current single-event test cannot verify ordering.

- [ ] **Step 1: Write the failing test**

Append to `pkg/orchestrator/events/events_test.go`, after `TestBus_SubscriberReceivesPublishedEvent`:

```go
func TestBus_DeliversEventsInPublishOrder(t *testing.T) {
    ctx := context.Background()
    bus := NewBus()
    sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 8, IncludeChunks: true})
    if err != nil {
        t.Fatalf("Subscribe() error = %v", err)
    }
    defer sub.Close()

    sequence := []EventType{
        EventTypeTaskCreated,
        EventTypeTaskRunning,
        EventTypeSessionOpened,
        EventTypeRequestCreated,
        EventTypeRequestRunning,
        EventTypeRequestTextChunk,
        EventTypeRequestCompleted,
        EventTypeSessionClosed,
    }
    for i, typ := range sequence {
        delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-1", "request-1", typ, uint64(i+1)))
        if delivered != 1 || dropped != 0 {
            t.Fatalf("Publish(%q) delivered/dropped = %d/%d, want 1/0", typ, delivered, dropped)
        }
    }

    for i, want := range sequence {
        got := receiveEvent(t, sub.Events())
        if got.Type != want {
            t.Fatalf("event[%d].Type = %q, want %q", i, got.Type, want)
        }
        if got.Seq != uint64(i+1) {
            t.Fatalf("event[%d].Seq = %d, want %d", i, got.Seq, i+1)
        }
    }
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./pkg/orchestrator/events -run TestBus_DeliversEventsInPublishOrder -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/events/events_test.go
git commit -m "test(events): assert publish-order delivery for multi-event subscriptions"
```

---

## Task 12: Harden `TestBus_ConcurrentPublishSubscribe` against the tight-buffer hang

**Files:**
- Modify: `pkg/orchestrator/events/events_test.go:248-292`

Background: `BufferSize = publishers * perPublisher` (= total events). If any event is dropped, the drain loop blocks on `receiveEvent` and hits its 1-second timeout — instead of asserting `Dropped() == 0` and producing a useful failure message. Two cheap fixes: (a) double the buffer so a sporadic stall can't fill it, (b) check `Dropped()` before the drain loop.

- [ ] **Step 1: Rewrite the test to check drops first**

In `pkg/orchestrator/events/events_test.go`, replace the body of `TestBus_ConcurrentPublishSubscribe` (lines 248-292):

```go
func TestBus_ConcurrentPublishSubscribe(t *testing.T) {
    ctx := context.Background()
    bus := NewBus()
    const subscribers = 8
    const publishers = 8
    const perPublisher = 25
    const totalEvents = publishers * perPublisher

    subs := make([]Subscription, 0, subscribers)
    for range subscribers {
        sub, err := bus.Subscribe(ctx, SubscribeOptions{
            TaskID:        "task-1",
            BufferSize:    totalEvents * 2, // headroom so a transient stall cannot cause drops
            IncludeChunks: true,
        })
        if err != nil {
            t.Fatalf("Subscribe() error = %v", err)
        }
        subs = append(subs, sub)
    }
    defer func() {
        for _, sub := range subs {
            if err := sub.Close(); err != nil {
                t.Errorf("Close() error = %v", err)
            }
        }
    }()

    var wg sync.WaitGroup
    for publisher := range publishers {
        wg.Add(1)
        go func(publisher int) {
            defer wg.Done()
            for seq := range perPublisher {
                event := testBusEvent("task-1", "session-1", "request-1", EventTypeRequestTextChunk, uint64(publisher*perPublisher+seq))
                bus.Publish(ctx, event)
            }
        }(publisher)
    }
    wg.Wait()

    // Check drop counters BEFORE draining, so an unexpected drop is reported
    // as a clean assertion failure instead of hanging on the drain loop.
    for i, sub := range subs {
        if dropped := sub.Dropped(); dropped != 0 {
            t.Fatalf("sub %d Dropped() = %d before drain, want 0", i, dropped)
        }
    }

    for i, sub := range subs {
        for range totalEvents {
            receiveEvent(t, sub.Events())
        }
        if dropped := sub.Dropped(); dropped != 0 {
            t.Fatalf("sub %d Dropped() = %d after drain, want 0", i, dropped)
        }
    }
}
```

- [ ] **Step 2: Run the test under race detector**

Run: `go test ./pkg/orchestrator/events -run TestBus_ConcurrentPublishSubscribe -race -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/orchestrator/events/events_test.go
git commit -m "test(events): harden ConcurrentPublishSubscribe against tight-buffer hangs"
```

---

## Task 13: Final verification

**Files:** (no edits — just gates)

- [ ] **Step 1: Format check**

Run: `gofmt -s -l pkg/orchestrator/audit pkg/orchestrator/events`
Expected: empty output (no unformatted files).

- [ ] **Step 2: Vet check**

Run: `go vet ./pkg/orchestrator/audit/... ./pkg/orchestrator/events/...`
Expected: clean.

- [ ] **Step 3: Full test pass with race detector**

Run: `go test ./pkg/orchestrator/audit/... ./pkg/orchestrator/events/... -race`
Expected: all PASS, no `DATA RACE` warnings.

- [ ] **Step 4: Confirm DESIGN / PLAN coherence**

Run: `grep -n "Seq" pkg/orchestrator/DESIGN.md pkg/orchestrator/audit/PLAN.md pkg/orchestrator/events/PLAN.md`
Expected: `Seq` appears in DESIGN §8.2 Event struct and in audit/PLAN.md EventRecord table; no stray `int` typing for `Seq`.

Run: `grep -nE "ErrNoSink|Dropped\(\)" pkg/orchestrator/audit/PLAN.md pkg/orchestrator/events/PLAN.md`
Expected: at least one match per term.

- [ ] **Step 5: Build the whole tree**

Run: `go build ./...`
Expected: clean build.

---

## Out of Scope

These were considered during review and explicitly deferred:

- **`events.Publish` not signalling empty-TaskID / cancelled-ctx ignores via logs.** Adding a `logs.FromContext(ctx).Warn(...)` would require pulling the `pkg/logs` dependency into a leaf package that today has zero non-stdlib non-idgen imports. The doc-only fix in Task 6 ("callers MUST validate task_id") is the minimum acceptable resolution. A follow-up can add logging when the engine wires the bus up and there's a natural caller-side audit hook.
- **Replacing the inline drop notification with a periodic ticker.** PLAN.md L69-70 originally promised "periodically (e.g., every 1 second per subscriber)" but the inline approach is cheaper (no extra goroutine per subscriber) and the latest-cumulative-count semantic preserves the user-visible guarantee. Task 6 aligns the doc to the code.
- **Cleaning up `cloneReflectValue` for typed-map edge cases.** The redaction round-trip already has the `TestRedactionPolicy_InPlaceMutationCannotCorruptStructuralMetadata` test covering nested `map[string][]string` and `[]map[string]any`. Further hardening (e.g., custom JSON round-trip cloning) would be a bigger refactor with no observed bug today.
