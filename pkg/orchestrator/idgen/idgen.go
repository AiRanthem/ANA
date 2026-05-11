// Package idgen provides identifier generators for orchestrator entities such as
// tasks, sessions, requests, and events. It exposes a deterministic sequential
// generator suitable for tests and a production-oriented default generator that
// combines a monotonic wall-clock component with crypto/rand entropy.
package idgen

import (
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type TaskID string
type SessionID string
type RequestID string

type Generator interface {
	NewTaskID() TaskID
	NewSessionID() SessionID
	NewRequestID() RequestID
	NewEventID() string
}

type sequentialGenerator struct {
	taskCounter    atomic.Uint64
	sessionCounter atomic.Uint64
	requestCounter atomic.Uint64
	eventCounter   atomic.Uint64
	taskPrefix     string
}

type defaultGenerator struct {
	mu           sync.Mutex
	lastUnixNano int64
}

func NewSequential(prefix string) Generator {
	return &sequentialGenerator{
		taskPrefix: prefix,
	}
}

func NewDefault() Generator {
	return &defaultGenerator{}
}

func formatSequential(prefix string, n uint64) string {
	return fmt.Sprintf("%s%010d", prefix, n)
}

func (g *sequentialGenerator) NewTaskID() TaskID {
	return TaskID(formatSequential(g.taskPrefix, g.taskCounter.Add(1)))
}

func (g *sequentialGenerator) NewSessionID() SessionID {
	return SessionID(formatSequential("S-", g.sessionCounter.Add(1)))
}

func (g *sequentialGenerator) NewRequestID() RequestID {
	return RequestID(formatSequential("R-", g.requestCounter.Add(1)))
}

func (g *sequentialGenerator) NewEventID() string {
	return formatSequential("E-", g.eventCounter.Add(1))
}

func (g *defaultGenerator) nextID() string {
	g.mu.Lock()
	now := time.Now().UnixNano()
	if now <= g.lastUnixNano {
		now = g.lastUnixNano + 1
	}
	g.lastUnixNano = now
	g.mu.Unlock()

	var entropy [5]byte
	n, err := rand.Read(entropy[:])
	if err != nil || n != len(entropy) {
		panic(fmt.Errorf("idgen default entropy: %w", err))
	}

	return fmt.Sprintf("%016x-%010x", uint64(now), entropy)
}

func (g *defaultGenerator) NewTaskID() TaskID {
	return TaskID(g.nextID())
}

func (g *defaultGenerator) NewSessionID() SessionID {
	return SessionID(g.nextID())
}

func (g *defaultGenerator) NewRequestID() RequestID {
	return RequestID(g.nextID())
}

func (g *defaultGenerator) NewEventID() string {
	return g.nextID()
}
