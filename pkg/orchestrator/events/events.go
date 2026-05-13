// Package events provides the orchestrator's in-process best-effort event bus.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AiRanthem/ANA/pkg/orchestrator/idgen"
)

var (
	ErrBusClosed            = errors.New("event bus closed")
	ErrSubscribeUnknownTask = errors.New("subscribe unknown task")
	ErrSubscriberLagged     = errors.New("subscriber lagged")
)

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

type Event struct {
	EventID    string
	TaskID     idgen.TaskID
	SessionID  idgen.SessionID
	RequestID  idgen.RequestID
	Type       EventType
	Seq        uint64
	OccurredAt time.Time
	Payload    any
}

type Bus interface {
	Publish(ctx context.Context, event Event) (delivered int, dropped int)
	Subscribe(ctx context.Context, options SubscribeOptions) (Subscription, error)
	Close(ctx context.Context) error
}

type SubscribeOptions struct {
	TaskID        idgen.TaskID
	SessionID     idgen.SessionID
	RequestID     idgen.RequestID
	BufferSize    int
	IncludeChunks bool
}

type Subscription interface {
	Events() <-chan Event
	Errors() <-chan error
	Dropped() uint64
	Close() error
}

type SubscriberLaggedError struct {
	TaskID     idgen.TaskID
	SessionID  idgen.SessionID
	RequestID  idgen.RequestID
	EventType  EventType
	EventSeq   uint64
	Dropped    uint64
	Subscriber string
}

func (e *SubscriberLaggedError) Error() string {
	return fmt.Sprintf("subscriber %s lagged for task %q event %q seq %d: dropped %d: %v", e.Subscriber, e.TaskID, e.EventType, e.EventSeq, e.Dropped, ErrSubscriberLagged)
}

func (e *SubscriberLaggedError) Unwrap() error {
	return ErrSubscriberLagged
}

type inProcessBus struct {
	mu              sync.Mutex
	closed          bool
	closeDone       chan struct{}
	activePublishes sync.WaitGroup
	nextSubID       uint64
	subscribers     map[idgen.TaskID]map[uint64]*subscription
	terminal        map[idgen.TaskID]struct{}
}

func NewBus() Bus {
	return &inProcessBus{
		closeDone:   make(chan struct{}),
		subscribers: make(map[idgen.TaskID]map[uint64]*subscription),
		terminal:    make(map[idgen.TaskID]struct{}),
	}
}

func (b *inProcessBus) Publish(ctx context.Context, event Event) (int, int) {
	if event.TaskID == "" {
		return 0, 0
	}
	terminal := isTerminalTaskEvent(event.Type)
	if ctx.Err() != nil && !terminal {
		return 0, 0
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, 0
	}
	b.activePublishes.Add(1)
	defer func() {
		b.activePublishes.Done()
		b.mu.Unlock()
	}()
	taskSubscribers := b.subscribers[event.TaskID]
	matchedSubscribers := make([]*subscription, 0, len(taskSubscribers))
	taskSubscribersAll := make([]*subscription, 0, len(taskSubscribers))
	for _, sub := range taskSubscribers {
		taskSubscribersAll = append(taskSubscribersAll, sub)
		if sub.matches(event) {
			matchedSubscribers = append(matchedSubscribers, sub)
		}
	}
	if terminal {
		b.terminal[event.TaskID] = struct{}{}
		delete(b.subscribers, event.TaskID)
	}

	delivered := 0
	dropped := 0
	for _, sub := range matchedSubscribers {
		switch sub.publish(snapshotEventPayload(event)) {
		case publishDelivered:
			delivered++
		case publishDropped:
			dropped++
		}
	}

	if terminal {
		for _, sub := range taskSubscribersAll {
			sub.close()
		}
	}
	return delivered, dropped
}

func snapshotEventPayload(event Event) Event {
	event.Payload = cloneEventPayload(event.Payload)
	return event
}

type payloadVisit struct {
	typ reflect.Type
	ptr uintptr
}

var jsonNumberType = reflect.TypeOf(json.Number(""))

func cloneEventPayload(payload any) any {
	if payload == nil {
		return nil
	}
	return clonePayloadValue(reflect.ValueOf(payload), make(map[payloadVisit]struct{})).Interface()
}

func clonePayloadValue(value reflect.Value, active map[payloadVisit]struct{}) reflect.Value {
	if !value.IsValid() {
		return value
	}
	if value.Type() == jsonNumberType {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := clonePayloadValue(value.Elem(), active)
		wrapped := reflect.New(value.Type()).Elem()
		wrapped.Set(compatiblePayloadValue(cloned, value.Type(), value.Elem()))
		return wrapped
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		if value.Type().Key().Kind() != reflect.String {
			return value
		}
		visit := payloadVisit{typ: value.Type(), ptr: value.Pointer()}
		if _, ok := active[visit]; ok {
			return value
		}
		active[visit] = struct{}{}
		defer delete(active, visit)

		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			clonedValue := clonePayloadValue(iter.Value(), active)
			cloned.SetMapIndex(iter.Key(), compatiblePayloadValue(clonedValue, value.Type().Elem(), iter.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := payloadVisit{typ: value.Type(), ptr: value.Pointer()}
		if visit.ptr != 0 {
			if _, ok := active[visit]; ok {
				return value
			}
			active[visit] = struct{}{}
			defer delete(active, visit)
		}

		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			clonedValue := clonePayloadValue(value.Index(i), active)
			cloned.Index(i).Set(compatiblePayloadValue(clonedValue, value.Type().Elem(), value.Index(i)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			clonedValue := clonePayloadValue(value.Index(i), active)
			cloned.Index(i).Set(compatiblePayloadValue(clonedValue, value.Type().Elem(), value.Index(i)))
		}
		return cloned
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return value
	default:
		return value
	}
}

func compatiblePayloadValue(value reflect.Value, target reflect.Type, fallback reflect.Value) reflect.Value {
	if !value.IsValid() {
		return fallback
	}
	if value.Type().AssignableTo(target) {
		return value
	}
	if value.Type().ConvertibleTo(target) {
		return value.Convert(target)
	}
	return fallback
}

func (b *inProcessBus) Subscribe(ctx context.Context, options SubscribeOptions) (Subscription, error) {
	if options.TaskID == "" {
		return nil, fmt.Errorf("subscribe task: %w", ErrSubscribeUnknownTask)
	}
	if options.BufferSize <= 0 {
		options.BufferSize = 256
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fmt.Errorf("subscribe task %q: %w", options.TaskID, ErrBusClosed)
	}
	if _, ended := b.terminal[options.TaskID]; ended {
		return nil, fmt.Errorf("subscribe task %q: %w", options.TaskID, ErrSubscribeUnknownTask)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("subscribe task %q: %w", options.TaskID, err)
	}

	b.nextSubID++
	sub := newSubscription(b, b.nextSubID, options)
	if b.subscribers[options.TaskID] == nil {
		b.subscribers[options.TaskID] = make(map[uint64]*subscription)
	}
	b.subscribers[options.TaskID][sub.id] = sub
	return sub, nil
}

func (b *inProcessBus) Close(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		closeDone := b.closeDone
		b.mu.Unlock()
		<-closeDone
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("close event bus: %w", err)
		}
		return nil
	}
	b.closed = true
	var subscribers []*subscription
	for _, taskSubscribers := range b.subscribers {
		for _, sub := range taskSubscribers {
			subscribers = append(subscribers, sub)
		}
	}
	b.subscribers = make(map[idgen.TaskID]map[uint64]*subscription)
	closeDone := b.closeDone
	b.mu.Unlock()

	b.activePublishes.Wait()
	for _, sub := range subscribers {
		sub.close()
	}
	close(closeDone)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("close event bus: %w", err)
	}
	return nil
}

func (b *inProcessBus) remove(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	taskSubscribers := b.subscribers[sub.options.TaskID]
	if taskSubscribers == nil {
		return
	}
	delete(taskSubscribers, sub.id)
	if len(taskSubscribers) == 0 {
		delete(b.subscribers, sub.options.TaskID)
	}
}

type subscription struct {
	bus     *inProcessBus
	id      uint64
	options SubscribeOptions
	events  chan Event
	errors  chan error
	dropped atomic.Uint64
	mu      sync.Mutex
	closed  bool
	once    sync.Once
}

func newSubscription(bus *inProcessBus, id uint64, options SubscribeOptions) *subscription {
	return &subscription{
		bus:     bus,
		id:      id,
		options: options,
		events:  make(chan Event, options.BufferSize),
		errors:  make(chan error, 1),
	}
}

func (s *subscription) Events() <-chan Event {
	return s.events
}

func (s *subscription) Errors() <-chan error {
	return s.errors
}

func (s *subscription) Dropped() uint64 {
	return s.dropped.Load()
}

func (s *subscription) Close() error {
	s.bus.remove(s)
	s.close()
	return nil
}

func (s *subscription) close() {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		close(s.events)
		close(s.errors)
	})
}

func (s *subscription) matches(event Event) bool {
	if event.TaskID != s.options.TaskID {
		return false
	}
	if s.options.SessionID != "" && event.SessionID != s.options.SessionID {
		return false
	}
	if s.options.RequestID != "" && event.RequestID != s.options.RequestID {
		return false
	}
	if !s.options.IncludeChunks && event.Type == EventTypeRequestTextChunk {
		return false
	}
	return true
}

type publishStatus int

const (
	publishInactive publishStatus = iota
	publishDelivered
	publishDropped
)

func (s *subscription) publish(event Event) publishStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return publishInactive
	}
	select {
	case s.events <- event:
		return publishDelivered
	default:
		dropped := s.dropped.Add(1)
		err := &SubscriberLaggedError{
			TaskID:     event.TaskID,
			SessionID:  event.SessionID,
			RequestID:  event.RequestID,
			EventType:  event.Type,
			EventSeq:   event.Seq,
			Dropped:    dropped,
			Subscriber: fmt.Sprintf("%d", s.id),
		}
		select {
		case s.errors <- err:
		default:
			select {
			case <-s.errors:
			default:
			}
			select {
			case s.errors <- err:
			default:
			}
		}
		return publishDropped
	}
}

func isTerminalTaskEvent(typ EventType) bool {
	switch typ {
	case EventTypeTaskCompleted, EventTypeTaskFailed, EventTypeTaskCancelled:
		return true
	default:
		return false
	}
}
