package protocol

import (
	"fmt"
	"sync"
)

// LifecycleTransition is a valid lifecycle state transition.
type LifecycleTransition struct {
	From StateStatus
	To   StateStatus
}

var lifecycleTransitions = []LifecycleTransition{
	{From: StateUninitialized, To: StatePreparingUV},
	{From: StateEnvironmentBroken, To: StatePreparingUV},
	{From: StateReadyToStart, To: StatePreparingUV},
	{From: StateEnvironmentBroken, To: StateSyncingEnvironment},
	{From: StateReadyToStart, To: StateSyncingEnvironment},
	{From: StatePreparingUV, To: StateSyncingRepository},
	{From: StatePreparingUV, To: StatePreparingPython},
	{From: StateSyncingRepository, To: StatePreparingPython},
	{From: StateSyncingRepository, To: StateEnvironmentBroken},
	{From: StatePreparingPython, To: StateSyncingEnvironment},
	{From: StatePreparingPython, To: StateEnvironmentBroken},
	{From: StateSyncingEnvironment, To: StateReadyToStart},
	{From: StateSyncingEnvironment, To: StateEnvironmentBroken},
	{From: StateReadyToStart, To: StateStartingBackend},
	{From: StateStartingBackend, To: StateRunning},
	{From: StateStartingBackend, To: StateStoppingBackend},
	{From: StateStartingBackend, To: StateBackendFailed},
	{From: StateRunning, To: StateStoppingBackend},
	{From: StateRunning, To: StateRestarting},
	{From: StateRunning, To: StateBackendFailed},
	{From: StateRestarting, To: StateRunning},
	{From: StateRestarting, To: StateStoppingBackend},
	{From: StateRestarting, To: StateBackendFailed},
	{From: StateStoppingBackend, To: StateStopped},
	{From: StateStoppingBackend, To: StateBackendFailed},
}

// AllLifecycleTransitions returns the lifecycle transition table in protocol order.
func AllLifecycleTransitions() []LifecycleTransition {
	return append([]LifecycleTransition(nil), lifecycleTransitions...)
}

// IsKnownLifecycleTransition reports whether from-to is in the static transition table.
func IsKnownLifecycleTransition(from, to StateStatus) bool {
	for _, transition := range lifecycleTransitions {
		if transition.From == from && transition.To == to {
			return true
		}
	}
	return false
}

// LifecycleMachine validates and applies in-process lifecycle transitions.
type LifecycleMachine struct {
	mu          sync.RWMutex
	initial     StateStatus
	current     StateStatus
	restartUsed bool
}

// NewLifecycleMachine creates a machine from a persisted stable lifecycle state.
func NewLifecycleMachine(initial StateStatus) (*LifecycleMachine, error) {
	switch initial {
	case StateUninitialized, StateEnvironmentBroken, StateReadyToStart:
		return &LifecycleMachine{
			initial: initial,
			current: initial,
		}, nil
	default:
		return nil, fmt.Errorf("invalid lifecycle initial state %q", initial)
	}
}

// Current returns the current lifecycle state.
func (m *LifecycleMachine) Current() StateStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Initial returns the stable state used to construct the machine.
func (m *LifecycleMachine) Initial() StateStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.initial
}

// RestartUsed reports whether the automatic backend restart has been used.
func (m *LifecycleMachine) RestartUsed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restartUsed
}

// Transition moves the machine to next when the static transition table allows it.
func (m *LifecycleMachine) Transition(next StateStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current := m.current
	if !IsKnownStateStatus(current) || !IsKnownStateStatus(next) {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: unknown state", current, next)
	}
	if current == next {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: self-loop", current, next)
	}
	if !IsKnownLifecycleTransition(current, next) {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: edge is not allowed", current, next)
	}

	m.current = next
	return nil
}
