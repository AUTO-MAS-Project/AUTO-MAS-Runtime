package protocol

import (
	"fmt"
	"sync"
)

// LifecycleTransition 表示一条合法的生命周期状态迁移。
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
	{From: StateUninitialized, To: StateSyncingRepository},
	{From: StateEnvironmentBroken, To: StateSyncingRepository},
	{From: StateReadyToStart, To: StateSyncingRepository},
	{From: StatePreparingUV, To: StateSyncingRepository},
	{From: StatePreparingUV, To: StatePreparingPython},
	{From: StatePreparingUV, To: StateEnvironmentBroken},
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

// AllLifecycleTransitions 按协议顺序返回生命周期迁移表的防御性副本。
func AllLifecycleTransitions() []LifecycleTransition {
	return append([]LifecycleTransition(nil), lifecycleTransitions...)
}

// IsKnownLifecycleTransition 报告 from 到 to 是否位于静态迁移表中。
func IsKnownLifecycleTransition(from, to StateStatus) bool {
	for _, transition := range lifecycleTransitions {
		if transition.From == from && transition.To == to {
			return true
		}
	}
	return false
}

// LifecycleMachine 校验并应用进程内生命周期迁移。
type LifecycleMachine struct {
	// mu 保护 initial、current 与 restartUsed，使读取和迁移处于同一线性化域。
	mu          sync.RWMutex
	initial     StateStatus
	current     StateStatus
	restartUsed bool
}

// NewLifecycleMachine 从已持久化的稳定生命周期状态创建状态机。
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

// Current 返回当前生命周期状态。
func (m *LifecycleMachine) Current() StateStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Initial 返回构造状态机时使用的稳定状态。
func (m *LifecycleMachine) Initial() StateStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.initial
}

// RestartUsed 报告自动后端重启机会是否已经使用。
func (m *LifecycleMachine) RestartUsed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restartUsed
}

// RollbackPreparation 在发生受管状态变更前恢复稳定初始状态。
func (m *LifecycleMachine) RollbackPreparation() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch m.current {
	case StatePreparingUV, StateSyncingRepository:
		m.current = m.initial
		return nil
	default:
		return fmt.Errorf("cannot roll back lifecycle preparation from %q", m.current)
	}
}

// Transition 在静态迁移表和当前实例的状态化守卫都允许时把状态机移动到 next。
// 状态化守卫拒绝终态迁移、自循环，以及绕过或重复使用单次自动重启。
func (m *LifecycleMachine) Transition(next StateStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current := m.current
	if !IsKnownStateStatus(current) || !IsKnownStateStatus(next) {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: unknown state", current, next)
	}
	if current == StateBackendFailed || current == StateStopped {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: current state is terminal", current, next)
	}
	if current == next {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: self-loop", current, next)
	}
	if !IsKnownLifecycleTransition(current, next) {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: edge is not allowed", current, next)
	}
	if current == StateRunning && next == StateRestarting {
		if m.restartUsed {
			return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: automatic backend restart already used", current, next)
		}
		m.restartUsed = true
	}
	if current == StateRunning && next == StateBackendFailed && !m.restartUsed {
		return fmt.Errorf("invalid lifecycle transition: from=%q to=%q: backend must use its automatic restart before failing", current, next)
	}

	m.current = next
	return nil
}
