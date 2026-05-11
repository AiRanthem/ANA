package idgen

import (
	"sync"
	"testing"
)

func TestSequentialGenerator_DeterministicIDs(t *testing.T) {
	t.Parallel()

	generator := NewSequential("T-")

	taskID1 := generator.NewTaskID()
	taskID2 := generator.NewTaskID()
	sessionID := generator.NewSessionID()
	requestID := generator.NewRequestID()
	eventID := generator.NewEventID()

	if taskID1 != TaskID("T-0000000001") {
		t.Fatalf("unexpected first task ID: %q", taskID1)
	}
	if taskID2 != TaskID("T-0000000002") {
		t.Fatalf("unexpected second task ID: %q", taskID2)
	}
	if sessionID != SessionID("S-0000000001") {
		t.Fatalf("unexpected session ID: %q", sessionID)
	}
	if requestID != RequestID("R-0000000001") {
		t.Fatalf("unexpected request ID: %q", requestID)
	}
	if eventID != "E-0000000001" {
		t.Fatalf("unexpected event ID: %q", eventID)
	}
}

func TestSequentialGenerator_CategoriesDoNotCollide(t *testing.T) {
	t.Parallel()

	generator := NewSequential("T-")

	ids := map[string]struct{}{
		string(generator.NewTaskID()):    {},
		string(generator.NewSessionID()): {},
		string(generator.NewRequestID()): {},
		generator.NewEventID():           {},
	}

	if len(ids) != 4 {
		t.Fatalf("expected 4 unique IDs, got %d", len(ids))
	}
}

func TestSequentialGenerator_ConcurrentUseIsUnique(t *testing.T) {
	t.Parallel()

	generator := NewSequential("T-")

	const count = 100

	ids := make(map[string]struct{}, count*4)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(count * 4)

	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			id := string(generator.NewTaskID())
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := string(generator.NewSessionID())
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := string(generator.NewRequestID())
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := generator.NewEventID()
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()
	}

	wg.Wait()

	if len(ids) != count*4 {
		t.Fatalf("expected %d unique IDs, got %d", count*4, len(ids))
	}
}

func TestDefaultGenerator_ReturnsNonEmptyUniqueIDs(t *testing.T) {
	t.Parallel()

	generator := NewDefault()

	ids := make(map[string]struct{}, 103)

	for i := 0; i < 100; i++ {
		id := string(generator.NewTaskID())
		if id == "" {
			t.Fatalf("empty task ID at index %d", i)
		}

		if _, exists := ids[id]; exists {
			t.Fatalf("duplicated task ID at index %d", i)
		}
		ids[id] = struct{}{}
	}

	sessionID := string(generator.NewSessionID())
	if sessionID == "" {
		t.Fatal("empty session ID")
	}
	if _, exists := ids[sessionID]; exists {
		t.Fatal("duplicated session ID")
	}
	ids[sessionID] = struct{}{}

	requestID := string(generator.NewRequestID())
	if requestID == "" {
		t.Fatal("empty request ID")
	}
	if _, exists := ids[requestID]; exists {
		t.Fatal("duplicated request ID")
	}
	ids[requestID] = struct{}{}

	eventID := generator.NewEventID()
	if eventID == "" {
		t.Fatal("empty event ID")
	}
	if _, exists := ids[eventID]; exists {
		t.Fatal("duplicated event ID")
	}
	ids[eventID] = struct{}{}
}

func TestDefaultGenerator_IDsAreASCIIAndReasonablySized(t *testing.T) {
	t.Parallel()

	generator := NewDefault()

	for i := 0; i < 100; i++ {
		id := string(generator.NewTaskID())
		if len(id) < 26 || len(id) > 36 {
			t.Fatalf("default task ID length out of range: %q (len=%d)", id, len(id))
		}
		for _, b := range []byte(id) {
			if b > 127 {
				t.Fatalf("default task ID contains non-ASCII byte: %q", b)
			}
		}
	}
}

func TestDefaultGenerator_IDsIncreaseMonotonically(t *testing.T) {
	t.Parallel()

	generator := NewDefault()

	previous := ""
	for i := 0; i < 100; i++ {
		current := string(generator.NewTaskID())
		if i > 0 && current <= previous {
			t.Fatalf("default task IDs are not lexicographically increasing: %q then %q", previous, current)
		}
		previous = current
	}
}

func TestDefaultGenerator_ConcurrentUseIsUnique(t *testing.T) {
	t.Parallel()

	generator := NewDefault()

	const count = 100

	ids := make(map[string]struct{}, count*4)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(count * 4)

	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			id := string(generator.NewTaskID())
			if id == "" {
				t.Error("empty task ID")
				return
			}
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := string(generator.NewSessionID())
			if id == "" {
				t.Error("empty session ID")
				return
			}
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := string(generator.NewRequestID())
			if id == "" {
				t.Error("empty request ID")
				return
			}
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()

		go func() {
			defer wg.Done()
			id := generator.NewEventID()
			if id == "" {
				t.Error("empty event ID")
				return
			}
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()
	}

	wg.Wait()

	if len(ids) != count*4 {
		t.Fatalf("expected %d unique IDs, got %d", count*4, len(ids))
	}
}
