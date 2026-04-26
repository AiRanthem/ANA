package agent

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// SpecSet stores AgentSpec values keyed by Type and is concurrency-safe.
type SpecSet struct {
	mu sync.RWMutex
	m  map[AgentType]AgentSpec
}

// NewSpecSet creates an empty spec set.
func NewSpecSet() *SpecSet {
	return &SpecSet{
		m: make(map[AgentType]AgentSpec),
	}
}

// Register stores a spec by its Type.
func (s *SpecSet) Register(spec AgentSpec) error {
	if spec == nil {
		return fmt.Errorf("%w: nil spec", ErrInvalidAgentSpec)
	}

	agentType := spec.Type()
	if err := ValidateAgentType(agentType); err != nil {
		return err
	}

	if spec.DisplayName() == "" {
		return fmt.Errorf("%w: empty display name for %q", ErrInvalidAgentSpec, agentType)
	}

	if spec.PluginLayout() == nil {
		return fmt.Errorf("%w: %q", ErrInvalidPluginLayout, agentType)
	}

	if err := ValidateProtocolDescriptor(spec.ProtocolDescriptor()); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.m[agentType]; ok {
		return fmt.Errorf("%w: %q", ErrAgentTypeConflict, agentType)
	}

	s.m[agentType] = spec
	return nil
}

// Get returns a registered spec by type.
func (s *SpecSet) Get(t AgentType) (AgentSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	spec, ok := s.m[t]
	return spec, ok
}

// Types returns a sorted snapshot of registered types.
func (s *SpecSet) Types() []AgentType {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]AgentType, 0, len(s.m))
	for agentType := range s.m {
		out = append(out, agentType)
	}
	slices.Sort(out)
	return out
}

// Len returns the number of registered specs.
func (s *SpecSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.m)
}

// Lookup returns the registered spec or ErrAgentTypeUnknown.
func (s *SpecSet) Lookup(t AgentType) (AgentSpec, error) {
	spec, ok := s.Get(t)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrAgentTypeUnknown, t)
	}
	return spec, nil
}

// IsTypeConflict reports whether err includes ErrAgentTypeConflict.
func IsTypeConflict(err error) bool {
	return errors.Is(err, ErrAgentTypeConflict)
}
