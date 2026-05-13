package events

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/orchestrator/idgen"
)

func TestBus_SubscriberReceivesPublishedEvent(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskCreated, 1))
	if delivered != 1 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 1/0", delivered, dropped)
	}

	got := receiveEvent(t, sub.Events())
	if got.Type != EventTypeTaskCreated || got.TaskID != "task-1" || got.Seq != 1 {
		t.Fatalf("event = %#v, want task.created for task-1 seq 1", got)
	}
}

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

func TestBus_MultipleSubscribersReceiveSameEvent(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	first, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("first Subscribe() error = %v", err)
	}
	defer first.Close()
	second, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("second Subscribe() error = %v", err)
	}
	defer second.Close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 2))
	if delivered != 2 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 2/0", delivered, dropped)
	}

	if got := receiveEvent(t, first.Events()); got.Type != EventTypeTaskRunning {
		t.Fatalf("first event type = %q, want %q", got.Type, EventTypeTaskRunning)
	}
	if got := receiveEvent(t, second.Events()); got.Type != EventTypeTaskRunning {
		t.Fatalf("second event type = %q, want %q", got.Type, EventTypeTaskRunning)
	}
}

func TestBus_SubscribeFiltersBySessionAndRequest(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{
		TaskID:     "task-1",
		SessionID:  "session-1",
		RequestID:  "request-1",
		BufferSize: 2,
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	if delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-2", "request-1", EventTypeRequestCreated, 1)); delivered != 0 || dropped != 0 {
		t.Fatalf("session mismatch delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
	if delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-1", "request-2", EventTypeRequestCreated, 2)); delivered != 0 || dropped != 0 {
		t.Fatalf("request mismatch delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
	if delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-1", "request-1", EventTypeRequestCreated, 3)); delivered != 1 || dropped != 0 {
		t.Fatalf("matching event delivered/dropped = %d/%d, want 1/0", delivered, dropped)
	}

	got := receiveEvent(t, sub.Events())
	if got.SessionID != "session-1" || got.RequestID != "request-1" || got.Seq != 3 {
		t.Fatalf("event = %#v, want matching session/request seq 3", got)
	}
	assertNoEvent(t, sub.Events())
}

func TestBus_SubscribeSuppressesChunksByDefault(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-1", "request-1", EventTypeRequestTextChunk, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("chunk Publish() delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
	assertNoEvent(t, sub.Events())
}

func TestBus_SubscribeIncludesChunksWhenRequested(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1, IncludeChunks: true})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "session-1", "request-1", EventTypeRequestTextChunk, 1))
	if delivered != 1 || dropped != 0 {
		t.Fatalf("chunk Publish() delivered/dropped = %d/%d, want 1/0", delivered, dropped)
	}
	if got := receiveEvent(t, sub.Events()); got.Type != EventTypeRequestTextChunk {
		t.Fatalf("event type = %q, want %q", got.Type, EventTypeRequestTextChunk)
	}
}

func TestBus_DefaultBufferSizeAllowsMultipleEvents(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	for seq := uint64(1); seq <= 2; seq++ {
		delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, seq))
		if delivered != 1 || dropped != 0 {
			t.Fatalf("Publish(seq=%d) delivered/dropped = %d/%d, want 1/0", seq, delivered, dropped)
		}
	}
	if got := receiveEvent(t, sub.Events()); got.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", got.Seq)
	}
	if got := receiveEvent(t, sub.Events()); got.Seq != 2 {
		t.Fatalf("second seq = %d, want 2", got.Seq)
	}
}

func TestBus_SubscribeEmptyTaskIDReturnsUnknownTask(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()

	_, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: ""})
	if !errors.Is(err, ErrSubscribeUnknownTask) {
		t.Fatalf("Subscribe() error = %v, want ErrSubscribeUnknownTask", err)
	}
}

func TestBus_SubscribeEmptyTaskIDWithCanceledContextReturnsUnknownTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bus := NewBus()

	_, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: ""})
	if !errors.Is(err, ErrSubscribeUnknownTask) {
		t.Fatalf("Subscribe() error = %v, want ErrSubscribeUnknownTask", err)
	}
}

func TestBus_PublishEmptyTaskIDReturnsZeroCounts(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()

	delivered, dropped := bus.Publish(ctx, testBusEvent("", "", "", EventTypeTaskRunning, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
}

func TestBus_SlowSubscriberDropsWithoutBlockingFastSubscriber(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	slow, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("slow Subscribe() error = %v", err)
	}
	defer slow.Close()
	fast, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 10})
	if err != nil {
		t.Fatalf("fast Subscribe() error = %v", err)
	}
	defer fast.Close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 1))
	if delivered != 2 || dropped != 0 {
		t.Fatalf("first Publish() delivered/dropped = %d/%d, want 2/0", delivered, dropped)
	}
	delivered, dropped = bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeRouteDirective, 2))
	if delivered != 1 || dropped != 1 {
		t.Fatalf("second Publish() delivered/dropped = %d/%d, want 1/1", delivered, dropped)
	}

	if got := receiveEvent(t, fast.Events()); got.Seq != 1 {
		t.Fatalf("fast first seq = %d, want 1", got.Seq)
	}
	if got := receiveEvent(t, fast.Events()); got.Seq != 2 {
		t.Fatalf("fast second seq = %d, want 2", got.Seq)
	}
	dropErr := receiveError(t, slow.Errors())
	var lagged *SubscriberLaggedError
	if !errors.As(dropErr, &lagged) {
		t.Fatalf("drop error = %v, want SubscriberLaggedError", dropErr)
	}
	if lagged.Dropped != 1 {
		t.Fatalf("lagged dropped = %d, want 1", lagged.Dropped)
	}
}

func TestBus_SlowSubscriberErrorsSurfaceLatestDropCount(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 2})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()

	bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 1))
	bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 2))
	for seq := uint64(3); seq <= 6; seq++ {
		bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeRouteDirective, seq))
	}

	dropErr := receiveError(t, sub.Errors())
	var lagged *SubscriberLaggedError
	if !errors.As(dropErr, &lagged) {
		t.Fatalf("drop error = %v, want SubscriberLaggedError", dropErr)
	}
	if lagged.Dropped != 4 || sub.Dropped() != 4 {
		t.Fatalf("drop counts error/sub = %d/%d, want 4/4", lagged.Dropped, sub.Dropped())
	}
	assertNoError(t, sub.Errors())
}

func TestBus_TerminalTaskEventClosesSubscription(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 2})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskCompleted, 1))
	if delivered != 1 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 1/0", delivered, dropped)
	}
	if got := receiveEvent(t, sub.Events()); got.Type != EventTypeTaskCompleted {
		t.Fatalf("event type = %q, want %q", got.Type, EventTypeTaskCompleted)
	}
	assertChannelClosed(t, sub.Events())

	_, err = bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if !errors.Is(err, ErrSubscribeUnknownTask) {
		t.Fatalf("Subscribe() after terminal error = %v, want ErrSubscribeUnknownTask", err)
	}
}

func TestBus_TerminalTaskEventWithCanceledContextStillClosesSubscription(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 2})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	delivered, dropped := bus.Publish(canceledCtx, testBusEvent("task-1", "", "", EventTypeTaskCompleted, 1))
	if delivered != 1 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 1/0", delivered, dropped)
	}
	if got := receiveEvent(t, sub.Events()); got.Type != EventTypeTaskCompleted {
		t.Fatalf("event type = %q, want %q", got.Type, EventTypeTaskCompleted)
	}
	assertChannelClosed(t, sub.Events())

	_, err = bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if !errors.Is(err, ErrSubscribeUnknownTask) {
		t.Fatalf("Subscribe() after terminal error = %v, want ErrSubscribeUnknownTask", err)
	}
}

func TestBus_TerminalTaskEventClosesScopedSubscription(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{
		TaskID:     "task-1",
		SessionID:  "session-1",
		RequestID:  "request-1",
		BufferSize: 1,
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskFailed, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 0/0 for scoped terminal close", delivered, dropped)
	}
	assertChannelClosed(t, sub.Events())
}

func TestSubscription_CloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := sub.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestBus_CloseClosesExistingSubscriber(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := bus.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertChannelClosed(t, sub.Events())
}

func TestBus_CloseWithCanceledContextStillClosesSubscribersAndRejectsFutureUse(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	err = bus.Close(canceledCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", err)
	}
	assertChannelClosed(t, sub.Events())

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("Publish() after canceled close delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
	_, err = bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Subscribe() after canceled close error = %v, want ErrBusClosed", err)
	}
}

func TestBus_ConcurrentCloseWaitsForSubscriberChannelsToClose(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	concreteBus := bus.(*inProcessBus)
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	concreteBus.activePublishes.Add(1)
	var releaseActivePublish sync.Once
	t.Cleanup(func() {
		releaseActivePublish.Do(concreteBus.activePublishes.Done)
	})
	firstCloseDone := make(chan error, 1)
	go func() {
		firstCloseDone <- bus.Close(ctx)
	}()
	waitForBusClosed(t, concreteBus)

	secondCloseDone := make(chan error, 1)
	go func() {
		err := bus.Close(ctx)
		select {
		case _, ok := <-sub.Events():
			if ok {
				t.Error("second Close returned before subscriber channel closed")
			}
		default:
			t.Error("second Close returned before subscriber channel closed")
		}
		secondCloseDone <- err
	}()

	for range 1000 {
		select {
		case err := <-secondCloseDone:
			t.Fatalf("second Close returned before in-flight publish completed: %v", err)
		default:
			runtime.Gosched()
		}
	}

	releaseActivePublish.Do(concreteBus.activePublishes.Done)
	if err := receiveCloseResult(t, firstCloseDone); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := receiveCloseResult(t, secondCloseDone); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestBus_PublishAfterCloseIsDeterministic(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskCreated, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("Publish() after close delivered/dropped = %d/%d, want 0/0", delivered, dropped)
	}
	_, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Subscribe() after close error = %v, want ErrBusClosed", err)
	}
}

func TestBus_SubscribeAfterCloseWithCanceledContextReturnsBusClosed(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err := bus.Subscribe(canceledCtx, SubscribeOptions{TaskID: "task-1"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Subscribe() after close error = %v, want ErrBusClosed", err)
	}
}

func TestBus_PublishIgnoresInactiveSubscriber(t *testing.T) {
	ctx := context.Background()
	bus := NewBus()
	sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	concrete := sub.(*subscription)
	concrete.close()

	delivered, dropped := bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeTaskRunning, 1))
	if delivered != 0 || dropped != 0 {
		t.Fatalf("Publish() delivered/dropped = %d/%d, want 0/0 for inactive subscriber", delivered, dropped)
	}
	if got := sub.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

func TestBus_PublishRacesWithSubscriptionAndBusClose(t *testing.T) {
	ctx := context.Background()

	t.Run("subscription close", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			bus := NewBus()
			sub, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1, IncludeChunks: true})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 10; seq++ {
					bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeRequestTextChunk, seq))
				}
			}()
			go func() {
				defer wg.Done()
				if err := sub.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			wg.Wait()
		}
	})

	t.Run("bus close", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			bus := NewBus()
			if _, err := bus.Subscribe(ctx, SubscribeOptions{TaskID: "task-1", BufferSize: 1, IncludeChunks: true}); err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 10; seq++ {
					bus.Publish(ctx, testBusEvent("task-1", "", "", EventTypeRequestTextChunk, seq))
				}
			}()
			go func() {
				defer wg.Done()
				if err := bus.Close(ctx); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			wg.Wait()
		}
	})
}

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

func testBusEvent(taskID idgen.TaskID, sessionID idgen.SessionID, requestID idgen.RequestID, typ EventType, seq uint64) Event {
	return Event{
		EventID:    "event",
		TaskID:     taskID,
		SessionID:  sessionID,
		RequestID:  requestID,
		Type:       typ,
		Seq:        seq,
		OccurredAt: time.Unix(0, int64(seq)),
		Payload:    map[string]any{"ok": true},
	}
}

func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed before event")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func receiveError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err, ok := <-ch:
		if !ok {
			t.Fatal("error channel closed before error")
		}
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func assertNoError(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err, ok := <-ch:
		if !ok {
			t.Fatal("error channel closed unexpectedly")
		}
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		t.Fatalf("unexpected event: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertChannelClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event channel still open")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event channel to close")
	}
}

func waitForBusClosed(t *testing.T, bus *inProcessBus) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		bus.mu.Lock()
		closed := bus.closed
		bus.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for bus to enter closing state")
		default:
			runtime.Gosched()
		}
	}
}

func receiveCloseResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Close result")
		return nil
	}
}
