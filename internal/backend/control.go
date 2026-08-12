package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

const (
	defaultControlMailboxCapacity = 64
	defaultShutdownTimeout        = 5 * time.Second
	defaultRestartDelay           = 2 * time.Second
	controlDrainTimeout           = time.Second
	backendCloseURL               = "http://127.0.0.1:36163/api/core/close"
)

var (
	// ErrControlStopped 表示控制 reader 已停止接受新命令。
	ErrControlStopped = errors.New("backend control mailbox is stopped")
	// ErrControlMailboxClosed 表示控制 mailbox 已永久收口。
	ErrControlMailboxClosed = errors.New("backend control mailbox is closed")
)

// ControlMailbox 是 ControlReader 与 Supervisor loop 之间的有界 FIFO。
// Submit 不执行进程或网络副作用；终止命令在入队时记录 first-wins latch。
type ControlMailbox struct {
	mu             sync.Mutex
	queue          []protocol.ControlCommand
	capacity       int
	wake           chan struct{}
	fullWaiters    chan struct{}
	stopped        bool
	closed         bool
	stopCh         chan struct{}
	closeCh        chan struct{}
	readerErr      error
	readerReported bool
	terminal       protocol.ControlCommand
	hasTerm        bool
	beforeShutdown func(string)
	setStage       func(protocol.Stage)
}

// NewControlMailbox 创建有界控制 FIFO；非正容量使用稳定默认值。
func NewControlMailbox(capacity int) *ControlMailbox {
	if capacity <= 0 {
		capacity = defaultControlMailboxCapacity
	}
	return &ControlMailbox{
		capacity:    capacity,
		wake:        make(chan struct{}),
		fullWaiters: make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
		closeCh:     make(chan struct{}),
	}
}

// Submit 将已校验命令入队；满队列时通过 ctx 背压而非丢弃命令。
func (m *ControlMailbox) Submit(ctx context.Context, command protocol.ControlCommand) error {
	if m == nil {
		return ErrControlMailboxClosed
	}
	if ctx == nil {
		return errors.New("backend control context is nil")
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return ErrControlMailboxClosed
		}
		if m.stopped {
			m.mu.Unlock()
			return ErrControlStopped
		}
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return err
		}
		if len(m.queue) < m.capacity {
			m.queue = append(m.queue, command)
			firstTerminal := false
			if command.Command == protocol.ControlCancel || command.Command == protocol.ControlShutdown {
				if !m.hasTerm {
					m.terminal = command
					m.hasTerm = true
					firstTerminal = true
				}
			}
			if command.Command == protocol.ControlShutdown && firstTerminal && !m.stopped {
				m.stopped = true
				close(m.stopCh)
			}
			m.signalLocked()
			m.mu.Unlock()
			return nil
		}
		wake, stopCh, closeCh := m.wake, m.stopCh, m.closeCh
		m.mu.Unlock()
		select {
		case m.fullWaiters <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopCh:
			return ErrControlStopped
		case <-closeCh:
			return ErrControlMailboxClosed
		case <-wake:
		}
	}
}

// Receive 按 FIFO 取出一个命令，并响应 ctx 取消。
func (m *ControlMailbox) Receive(ctx context.Context) (protocol.ControlCommand, error) {
	if m == nil {
		return protocol.ControlCommand{}, ErrControlMailboxClosed
	}
	if ctx == nil {
		return protocol.ControlCommand{}, errors.New("backend control context is nil")
	}
	for {
		m.mu.Lock()
		if len(m.queue) > 0 {
			command := m.queue[0]
			m.queue = m.queue[1:]
			if command.Command == protocol.ControlCancel || command.Command == protocol.ControlShutdown {
				if !m.hasTerm {
					m.terminal = command
					m.hasTerm = true
				}
			}
			m.signalLocked()
			m.mu.Unlock()
			return command, nil
		}
		if m.readerErr != nil && !m.readerReported {
			m.readerReported = true
			err := m.readerErr
			m.mu.Unlock()
			return protocol.ControlCommand{}, err
		}
		if m.stopped {
			m.mu.Unlock()
			return protocol.ControlCommand{}, ErrControlStopped
		}
		if m.closed {
			m.mu.Unlock()
			return protocol.ControlCommand{}, ErrControlMailboxClosed
		}
		wake, closeCh := m.wake, m.closeCh
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return protocol.ControlCommand{}, ctx.Err()
		case <-wake:
			continue
		case <-closeCh:
			continue
		}
	}
}

// SetReaderError 将控制 reader 的基础设施故障交给 Supervisor 消费，
// 使其先收口受管进程再由 CLI 分类最终错误。
func (m *ControlMailbox) SetReaderError(err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	if m.readerErr == nil {
		m.readerErr = err
		m.signalLocked()
	}
	m.mu.Unlock()
}

// InfrastructureError 返回尚未被 Receive 消费的 reader 故障。
func (m *ControlMailbox) InfrastructureError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readerErr
}

// StopAccepting 先唤醒背压中的 producer，再阻止后续命令进入 FIFO。
func (m *ControlMailbox) StopAccepting() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stopCh)
		m.signalLocked()
	}
	m.mu.Unlock()
}

// Close 终止 mailbox；已入队命令仍可被 Receive 消费一次。
func (m *ControlMailbox) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.stopped = true
		close(m.closeCh)
		select {
		case <-m.stopCh:
		default:
			close(m.stopCh)
		}
		m.signalLocked()
	}
	m.mu.Unlock()
}

func (m *ControlMailbox) signalLocked() {
	close(m.wake)
	m.wake = make(chan struct{})
}

// TerminalCommand 返回入队时记录的 first-wins 终止命令。
func (m *ControlMailbox) TerminalCommand() (protocol.ControlCommand, bool) {
	if m == nil {
		return protocol.ControlCommand{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terminal, m.hasTerm
}

// SetBeforeShutdown 注册 shutdown 被消费前的 reader 停止回调。
func (m *ControlMailbox) SetBeforeShutdown(callback func(string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.beforeShutdown = callback
	m.mu.Unlock()
}

// BeforeShutdown 返回当前 shutdown gate 回调。
func (m *ControlMailbox) BeforeShutdown(commandID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	callback := m.beforeShutdown
	m.mu.Unlock()
	if callback != nil {
		callback(commandID)
	}
}

// SetStageCallback 注册当前 backend stage 的只读同步回调。
func (m *ControlMailbox) SetStageCallback(callback func(protocol.Stage)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.setStage = callback
	m.mu.Unlock()
}

// SetStage 将 backend stage 转发给控制 warning 的消费侧。
func (m *ControlMailbox) SetStage(stage protocol.Stage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	callback := m.setStage
	m.mu.Unlock()
	if callback != nil {
		callback(stage)
	}
}

var _ ControlReceiver = (*ControlMailbox)(nil)

type controlAttempt struct {
	process ManagedProcess
	tx      TransactionHandle
	logger  Logger
	gate    *streamGate
	stage   protocol.Stage
	results <-chan controlResult
}

type controlResult struct {
	command protocol.ControlCommand
	err     error
}

type controlInterruptError struct {
	command protocol.ControlCommand
	attempt *controlAttempt
	cause   error
}

func (e *controlInterruptError) Error() string {
	if e == nil {
		return "backend control interrupt"
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return "backend control interrupt"
}

func (e *controlInterruptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type controlState struct {
	mu               sync.Mutex
	stage            protocol.Stage
	status           protocol.StateStatus
	details          map[string]any
	closeResources   func() error
	controlResults   <-chan controlResult
	controlCloseOnce sync.Once
	controlCloseErr  error
	controlClosed    bool
}

func (s *controlState) set(stage protocol.Stage, status protocol.StateStatus, details map[string]any) {
	s.mu.Lock()
	s.stage = stage
	s.status = status
	s.details = cloneControlDetails(details)
	s.mu.Unlock()
}

func (s *controlState) snapshot() (protocol.Stage, protocol.StateStatus, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.status, cloneControlDetails(s.details)
}

func (s *controlState) finalizeResources() error {
	if s == nil || s.closeResources == nil {
		return nil
	}
	return s.closeResources()
}

func (s *controlState) markControlClosed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.controlClosed = true
	s.mu.Unlock()
}

func cloneControlDetails(details map[string]any) map[string]any {
	copyOf := make(map[string]any, len(details))
	for key, value := range details {
		copyOf[key] = value
	}
	return copyOf
}

// emitControlFailure 先把已提交监督的失败写入生命周期，再把原始 typed error 交给 CLI。
func (s *ManagedSupervisor) emitControlFailure(request Request, snapshot *controlState, err error) error {
	if err == nil {
		return nil
	}
	if snapshot != nil {
		if drainErr := s.closeControlBeforeFailure(request, snapshot, backendErrorHasCode(err, protocol.CodeOutputWriteFailed)); drainErr != nil {
			err = errors.Join(drainErr, err)
		}
	}
	details := map[string]any{}
	for key, value := range nestedControlDetails(err) {
		details[key] = value
	}
	stage := protocol.StageBackendCleanup
	if snapshot != nil {
		snapshot.set(stage, protocol.StateBackendFailed, details)
	}
	setControlStage(request.Control, stage)
	stateErr := s.emitState(request.Emitter, stage, protocol.StateBackendFailed, "后端监督失败", details)
	return errors.Join(err, stateErr)
}

func (s *ManagedSupervisor) closeControlBeforeFailure(request Request, snapshot *controlState, outputFault bool) error {
	if snapshot == nil {
		return nil
	}
	snapshot.controlCloseOnce.Do(func() {
		snapshot.mu.Lock()
		alreadyClosed := snapshot.controlClosed
		results := snapshot.controlResults
		snapshot.mu.Unlock()
		if alreadyClosed {
			return
		}
		before := request.BeforeControlClose
		if before == nil {
			if stopper, ok := request.Control.(interface{ StopAccepting() }); ok {
				before = stopper.StopAccepting
			}
		}
		if before != nil {
			before()
		}
		snapshot.markControlClosed()
		if results == nil || outputFault {
			return
		}
		drainCtx, cancel := context.WithTimeout(context.Background(), controlDrainTimeout)
		defer cancel()
		for {
			select {
			case result := <-results:
				if result.err != nil {
					if errors.Is(result.err, ErrControlStopped) || errors.Is(result.err, ErrControlMailboxClosed) {
						return
					}
					snapshot.controlCloseErr = controlResultError(snapshot, result.err)
					return
				}
				if outputFault || result.command.Command != protocol.ControlStatus {
					continue
				}
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					snapshot.controlCloseErr = err
					return
				}
			case <-drainCtx.Done():
				snapshot.controlCloseErr = drainCtx.Err()
				return
			}
		}
	})
	return snapshot.controlCloseErr
}

func (s *ManagedSupervisor) closeControlAndDrain(request Request, snapshot *controlState, before func()) error {
	if snapshot == nil {
		return nil
	}
	snapshot.controlCloseOnce.Do(func() {
		snapshot.mu.Lock()
		alreadyClosed := snapshot.controlClosed
		results := snapshot.controlResults
		snapshot.mu.Unlock()
		if alreadyClosed {
			return
		}
		if before != nil {
			before()
		} else if stopper, ok := request.Control.(interface{ StopAccepting() }); ok {
			stopper.StopAccepting()
		}
		snapshot.markControlClosed()
		if results == nil {
			return
		}
		drainCtx, cancel := context.WithTimeout(context.Background(), controlDrainTimeout)
		defer cancel()
		for {
			select {
			case result := <-results:
				if result.err != nil {
					if errors.Is(result.err, ErrControlStopped) || errors.Is(result.err, ErrControlMailboxClosed) {
						return
					}
					snapshot.controlCloseErr = controlResultError(snapshot, result.err)
					return
				}
				if result.command.Command != protocol.ControlStatus {
					continue
				}
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					snapshot.controlCloseErr = err
					return
				}
			case <-drainCtx.Done():
				snapshot.controlCloseErr = drainCtx.Err()
				return
			}
		}
	})
	return snapshot.controlCloseErr
}

func controlResultError(snapshot *controlState, err error) error {
	if err == nil {
		return nil
	}
	return newError(protocol.CodeInternalError, controlSnapshotStage(snapshot, protocol.StageBackendCleanup), "stdin 控制通道读取失败", nil, err)
}

func (s *ManagedSupervisor) closeControlBeforeCancel(request Request, snapshot *controlState) error {
	return s.closeControlAndDrain(request, snapshot, request.BeforeControlClose)
}

func (s *ManagedSupervisor) closeControlBeforeShutdown(request Request, snapshot *controlState, commandID string) error {
	var before func()
	if request.BeforeShutdown != nil {
		before = func() { request.BeforeShutdown(commandID) }
	}
	return s.closeControlAndDrain(request, snapshot, before)
}

func nestedControlDetails(err error) map[string]any {
	details := make(map[string]any)
	collectControlDetails(err, details)
	return details
}

func restartFailureDetails(previous map[string]any, err error) map[string]any {
	details := cloneControlDetails(previous)
	child := nestedControlDetails(err)
	if previousPID, ok := details["pid"]; ok {
		if _, hasCurrent := child["pid"]; hasCurrent {
			details["previousPid"] = previousPID
			delete(details, "pid")
		}
	}
	if previousLogPath, ok := details["logPath"]; ok {
		if _, hasCurrent := child["logPath"]; hasCurrent {
			details["previousLogPath"] = previousLogPath
			delete(details, "logPath")
		}
	}
	if previousExitCode, ok := details["exitCode"]; ok {
		if _, hasCurrent := child["exitCode"]; hasCurrent {
			details["previousExitCode"] = previousExitCode
			delete(details, "exitCode")
		}
	}
	for key, value := range child {
		details[key] = value
	}
	return details
}

func collectControlDetails(err error, details map[string]any) {
	if err == nil {
		return
	}
	var coded interface{ Details() map[string]any }
	if errors.As(err, &coded) {
		for key, value := range coded.Details() {
			if _, exists := details[key]; !exists {
				details[key] = value
			}
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			collectControlDetails(child, details)
		}
		return
	}
	if unwrapped, ok := err.(interface{ Unwrap() error }); ok {
		collectControlDetails(unwrapped.Unwrap(), details)
	}
}

func backendErrorCommitted(err error) bool {
	if err == nil {
		return false
	}
	var committed interface{ Committed() bool }
	return errors.As(err, &committed) && committed.Committed()
}

func backendErrorHasCode(err error, want protocol.Code) bool {
	if err == nil {
		return false
	}
	var coded interface{ Code() protocol.Code }
	if errors.As(err, &coded) && coded.Code() == want {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if backendErrorHasCode(child, want) {
				return true
			}
		}
		return false
	}
	if unwrapped, ok := err.(interface{ Unwrap() error }); ok {
		return backendErrorHasCode(unwrapped.Unwrap(), want)
	}
	return false
}

// superviseControlled 是 T6.4 的长驻控制路径；旧的无 Control 请求继续走 T6.3 路径。
func (s *ManagedSupervisor) superviseControlled(ctx context.Context, request Request) (returnErr error) {
	forwarderCtx, cancelForwarder := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelForwarder()
	results, readerDone := startControlForwarder(forwarderCtx, request.Control)
	defer func() {
		cancelForwarder()
		if readerDone != nil {
			<-readerDone
		}
	}()
	stateSnapshot := &controlState{controlResults: results}
	stateSnapshot.set(protocol.StageBackendSpawn, protocol.StateReadyToStart, map[string]any{})

	locks, err := s.deps.Lock.Acquire(ctx)
	if err != nil || locks == nil {
		if err == nil {
			err = errors.New("backend lock lease is nil")
		}
		primary := preferCancellation(ctx, mapDependencyError(protocol.StageBackendSpawn, protocol.CodeMutexOperationFailed, "后端 Mutex 获取失败", err))
		var resourceErr error
		resourceErr = errors.Join(resourceErr, mapStateCleanupError(s.deps.State.Close()))
		if locks != nil {
			resourceErr = errors.Join(resourceErr, mapMutexCleanupError(locks.Close()))
		}
		resourceErr = errors.Join(resourceErr, mapMutexCleanupError(s.deps.Lock.Close()))
		if handled, terminalErr := s.finishLatchedControl(ctx, request, stateSnapshot, nil, results); handled {
			return errors.Join(resourceErr, terminalErr)
		}
		closeErr := s.closeControlBeforeFailure(request, stateSnapshot, backendErrorHasCode(primary, protocol.CodeOutputWriteFailed))
		return errors.Join(resourceErr, closeErr, primary)
	}
	var closeResourcesOnce sync.Once
	var closeResourcesErr error
	closeResources := func() error {
		closeResourcesOnce.Do(func() {
			closeResourcesErr = errors.Join(
				mapStateCleanupError(s.deps.State.Close()),
				mapMutexCleanupError(locks.Close()),
				mapMutexCleanupError(s.deps.Lock.Close()),
			)
		})
		return closeResourcesErr
	}
	defer func() {
		returnErr = errors.Join(returnErr, closeResources())
	}()

	stateSnapshot.closeResources = closeResources
	environment, revision, err := s.awaitControlPreflight(ctx, request, stateSnapshot, results)
	if err != nil {
		var interrupted *controlInterruptError
		if errors.As(err, &interrupted) {
			if interrupted.command.Command == protocol.ControlCancel {
				cancelled := newControlCancelled(interrupted.command.CommandID)
				return errors.Join(s.closeControlBeforeCancel(request, stateSnapshot), cancelled)
			}
			return s.finishControlShutdownInterrupt(ctx, request, stateSnapshot, interrupted.command.CommandID)
		}
		if backendErrorHasCode(err, protocol.CodeOutputWriteFailed) {
			_, status, _ := stateSnapshot.snapshot()
			if status == protocol.StateReadyToStart {
				closeErr := s.closeControlBeforeFailure(request, stateSnapshot, true)
				return errors.Join(closeErr, err)
			}
			return s.emitControlFailure(request, stateSnapshot, err)
		}
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			failure := newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
			closeErr := s.closeControlBeforeFailure(request, stateSnapshot, false)
			return errors.Join(closeErr, failure, err)
		}
		if handled, terminalErr := s.finishLatchedControl(ctx, request, stateSnapshot, nil, results); handled {
			return terminalErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			closeErr := s.closeControlBeforeFailure(request, stateSnapshot, backendErrorHasCode(err, protocol.CodeOutputWriteFailed))
			return errors.Join(closeErr, ctxErr)
		}
		closeErr := s.closeControlBeforeFailure(request, stateSnapshot, backendErrorHasCode(err, protocol.CodeOutputWriteFailed))
		return errors.Join(closeErr, err)
	}
	_ = environment
	attempt, err := s.startControlAttempt(ctx, request, environment, revision, stateSnapshot, false, results)
	if err != nil {
		var interrupted *controlInterruptError
		if errors.As(err, &interrupted) {
			if interrupted.attempt == nil {
				if interrupted.command.Command == protocol.ControlCancel {
					cancelled := newControlCancelled(interrupted.command.CommandID)
					return errors.Join(s.closeControlBeforeCancel(request, stateSnapshot), cancelled)
				}
				return s.finishControlShutdownInterrupt(ctx, request, stateSnapshot, interrupted.command.CommandID)
			}
			if interrupted.command.Command == protocol.ControlCancel {
				return s.finishControlCancel(ctx, request, stateSnapshot, interrupted.attempt, interrupted.command.CommandID)
			}
			return s.finishControlShutdown(ctx, request, interrupted.attempt, stateSnapshot, interrupted.command.CommandID)
		}
		if handled, resolved := s.resolveStartAttemptError(ctx, request, stateSnapshot, err, false, nil, results); handled {
			return resolved
		}
		closeErr := s.closeControlBeforeFailure(request, stateSnapshot, backendErrorHasCode(err, protocol.CodeOutputWriteFailed))
		return errors.Join(closeErr, err)
	}
	restartUsed := false
	var restartFacts map[string]any
	for {
		if fault := attempt.gate.Fault(); fault != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
			return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
		}
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
			failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
			return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
		}
		if ctx.Err() != nil {
			if command, ok := terminalCommand(request.Control); ok {
				if command.Command == protocol.ControlShutdown {
					return s.finishControlShutdown(ctx, request, attempt, stateSnapshot, command.CommandID)
				}
				return s.finishControlCancel(ctx, request, stateSnapshot, attempt, command.CommandID)
			}
			return s.finishControlCancel(ctx, request, stateSnapshot, attempt, "")
		}
		select {
		case <-attempt.gate.Faulted():
			fault := attempt.gate.Fault()
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
			return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
		case <-ctx.Done():
			if fault := attempt.gate.Fault(); fault != nil {
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
				return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
				failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
				return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
			}
			if command, ok := terminalCommand(request.Control); ok {
				if command.Command == protocol.ControlShutdown {
					return s.finishControlShutdown(ctx, request, attempt, stateSnapshot, command.CommandID)
				}
				return s.finishControlCancel(ctx, request, stateSnapshot, attempt, command.CommandID)
			}
			return s.finishControlCancel(ctx, request, stateSnapshot, attempt, "")
		case <-attempt.process.Exited():
			if fault := attempt.gate.Fault(); fault != nil {
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
				return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
				failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
				return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, stateSnapshot, results)
				if drainErr != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, drainErr))
				}
				if command.Command == protocol.ControlCancel {
					return s.finishControlCancel(ctx, request, stateSnapshot, attempt, command.CommandID)
				}
				return s.finishControlShutdown(ctx, request, attempt, stateSnapshot, command.CommandID)
			}
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
			gateFault := attempt.gate.Fault()
			attempt = nil
			restartFacts = cloneControlDetails(cleanup.details)
			if gateFault != nil {
				return s.emitControlFailure(request, stateSnapshot, errors.Join(gateFault, cleanup.err))
			}
			if cleanup.err != nil {
				return s.emitControlFailure(request, stateSnapshot, cleanup.err)
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return s.emitControlFailure(request, stateSnapshot, newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr))
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, stateSnapshot, results)
				if drainErr != nil {
					return s.emitControlFailure(request, stateSnapshot, drainErr)
				}
				if command.Command == protocol.ControlCancel {
					return s.finishControlCancel(ctx, request, stateSnapshot, nil, command.CommandID)
				}
				return s.finishControlShutdown(ctx, request, nil, stateSnapshot, command.CommandID)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return s.finishControlCancel(ctx, request, stateSnapshot, nil, "")
			}
			if restartUsed {
				return s.emitControlFailure(request, stateSnapshot, newCommittedError(protocol.CodeBackendExitedUnexpectedly, protocol.StageBackendRun, "后端意外退出", cleanup.details, nil))
			}
			restartUsed = true
			stateSnapshot.set(protocol.StageBackendRestart, protocol.StateRestarting, map[string]any{})
			if err := s.emitState(request.Emitter, protocol.StageBackendRestart, protocol.StateRestarting, "后端正在重启", map[string]any{}); err != nil {
				return err
			}
			if err := s.waitRestart(ctx, request, stateSnapshot, results); err != nil {
				var interrupted *controlInterruptError
				if errors.As(err, &interrupted) {
					if interrupted.command.Command == protocol.ControlCancel {
						return s.finishControlCancel(ctx, request, stateSnapshot, nil, interrupted.command.CommandID)
					}
					return s.finishControlShutdown(ctx, request, nil, stateSnapshot, interrupted.command.CommandID)
				}
				if backendErrorHasCode(err, protocol.CodeOutputWriteFailed) {
					return s.emitControlFailure(request, stateSnapshot, err)
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return s.emitControlFailure(request, stateSnapshot, err)
				}
				if handled, terminalErr := s.finishLatchedControl(ctx, request, stateSnapshot, nil, results); handled {
					return terminalErr
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return s.finishControlCancel(ctx, request, stateSnapshot, nil, "")
				}
				return s.emitControlFailure(request, stateSnapshot, newError(protocol.CodeBackendRestartFailed, protocol.StageBackendRestart, "后端重启等待失败", restartFailureDetails(restartFacts, err), err))
			}
			environment, revision, err = s.awaitControlPreflight(ctx, request, stateSnapshot, results)
			if err != nil {
				var interrupted *controlInterruptError
				if errors.As(err, &interrupted) {
					if interrupted.attempt == nil {
						if interrupted.command.Command == protocol.ControlCancel {
							cancelled := newControlCancelled(interrupted.command.CommandID)
							return errors.Join(s.closeControlBeforeCancel(request, stateSnapshot), cancelled)
						}
						return s.finishControlShutdownInterrupt(ctx, request, stateSnapshot, interrupted.command.CommandID)
					}
					if interrupted.command.Command == protocol.ControlCancel {
						return s.finishControlCancel(ctx, request, stateSnapshot, nil, interrupted.command.CommandID)
					}
					return s.finishControlShutdown(ctx, request, nil, stateSnapshot, interrupted.command.CommandID)
				}
				if backendErrorHasCode(err, protocol.CodeOutputWriteFailed) {
					return s.emitControlFailure(request, stateSnapshot, err)
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return s.emitControlFailure(request, stateSnapshot, err)
				}
				if handled, terminalErr := s.finishLatchedControl(ctx, request, stateSnapshot, nil, results); handled {
					return terminalErr
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return s.finishControlCancel(ctx, request, stateSnapshot, nil, "")
				}
				return s.emitControlFailure(request, stateSnapshot, newError(protocol.CodeBackendRestartFailed, protocol.StageBackendRestart, "后端重启前置检查失败", restartFailureDetails(restartFacts, err), err))
			}
			attempt, err = s.startControlAttempt(ctx, request, environment, revision, stateSnapshot, true, results)
			if err != nil {
				var interrupted *controlInterruptError
				if errors.As(err, &interrupted) {
					if interrupted.attempt == nil {
						if interrupted.command.Command == protocol.ControlCancel {
							cancelled := newControlCancelled(interrupted.command.CommandID)
							return errors.Join(s.closeControlBeforeCancel(request, stateSnapshot), cancelled)
						}
						return s.finishControlShutdownInterrupt(ctx, request, stateSnapshot, interrupted.command.CommandID)
					}
					if interrupted.command.Command == protocol.ControlCancel {
						return s.finishControlCancel(ctx, request, stateSnapshot, interrupted.attempt, interrupted.command.CommandID)
					}
					return s.finishControlShutdown(ctx, request, interrupted.attempt, stateSnapshot, interrupted.command.CommandID)
				}
				if handled, resolved := s.resolveStartAttemptError(ctx, request, stateSnapshot, err, true, restartFacts, results); handled {
					return resolved
				}
				return s.emitControlFailure(request, stateSnapshot, newError(protocol.CodeBackendRestartFailed, protocol.StageBackendRestart, "后端重启失败", restartFailureDetails(restartFacts, err), err))
			}
		case result := <-results:
			if result.err != nil {
				if fault := attempt.gate.Fault(); fault != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
				}
				if handled, terminalErr := s.finishLatchedControl(ctx, request, stateSnapshot, attempt, results); handled {
					return terminalErr
				}
				if ctx.Err() != nil {
					return s.finishControlCancel(ctx, request, stateSnapshot, attempt, "")
				}
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
				failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, result.err)
				return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, details := stateSnapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, err))
				}
			case protocol.ControlCancel:
				if fault := attempt.gate.Fault(); fault != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
				}
				return s.finishControlCancel(ctx, request, stateSnapshot, attempt, result.command.CommandID)
			case protocol.ControlShutdown:
				if fault := attempt.gate.Fault(); fault != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, fault))
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
					failure := newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr)
					return s.emitControlFailure(request, stateSnapshot, errors.Join(cleanup.err, failure))
				}
				return s.finishControlShutdown(ctx, request, attempt, stateSnapshot, result.command.CommandID)
			}
		}
	}
}

func (s *ManagedSupervisor) awaitControlPreflight(ctx context.Context, request Request, snapshot *controlState, results <-chan controlResult) (state.EnvironmentState, state.Revision, error) {
	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		environment state.EnvironmentState
		revision    state.Revision
		err         error
	}
	done := make(chan result, 1)
	go func() {
		environment, revision, err := s.controlPreflight(phaseCtx, request)
		done <- result{environment: environment, revision: revision, err: err}
	}()
	for {
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			cancel()
			<-done
			return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
		}
		select {
		case result := <-done:
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
				if drainErr != nil {
					return state.EnvironmentState{}, state.Revision{}, drainErr
				}
				return state.EnvironmentState{}, state.Revision{}, &controlInterruptError{command: command}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return state.EnvironmentState{}, state.Revision{}, ctxErr
			}
			return result.environment, result.revision, result.err
		case <-ctx.Done():
			cancel()
			<-done
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
				if drainErr != nil {
					return state.EnvironmentState{}, state.Revision{}, drainErr
				}
				return state.EnvironmentState{}, state.Revision{}, &controlInterruptError{command: command}
			}
			return state.EnvironmentState{}, state.Revision{}, ctx.Err()
		case result := <-results:
			if result.err != nil {
				cancel()
				<-done
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
				}
				if _, ok := terminalCommand(request.Control); ok {
					command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
					if drainErr != nil {
						return state.EnvironmentState{}, state.Revision{}, drainErr
					}
					return state.EnvironmentState{}, state.Revision{}, &controlInterruptError{command: command}
				}
				return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, result.err)
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					cancel()
					<-done
					return state.EnvironmentState{}, state.Revision{}, err
				}
			case protocol.ControlCancel, protocol.ControlShutdown:
				cancel()
				<-done
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
				}
				return state.EnvironmentState{}, state.Revision{}, &controlInterruptError{command: result.command}
			}
		}
	}
}

func (s *ManagedSupervisor) controlPreflight(ctx context.Context, request Request) (state.EnvironmentState, state.Revision, error) {
	if modeForRequest(request) == ModeDevelopment {
		if _, err := s.normalizeDevelopmentRequest(ctx, request); err != nil {
			return state.EnvironmentState{}, state.Revision{}, err
		}
	}
	if err := s.recoverStaleTransaction(ctx); err != nil {
		return state.EnvironmentState{}, state.Revision{}, err
	}
	if modeForRequest(request) == ModeDevelopment {
		if err := s.deps.UV.Check(ctx, uv.RunOptions{
			Stage:         protocol.StageBackendSpawn,
			ProjectDir:    request.DevelopmentRepo,
			ProjectEnvDir: developmentProjectEnv(request.DevelopmentRepo),
		}); err != nil {
			return state.EnvironmentState{}, state.Revision{}, mapDependencyError(protocol.StageBackendSpawn, protocol.CodeUVExecFailed, "开发模式 uv 校验失败", err)
		}
		return state.EnvironmentState{}, state.Revision{}, nil
	}
	if err := s.checkEntry(ctx); err != nil {
		return state.EnvironmentState{}, state.Revision{}, err
	}
	environment, err := s.deps.State.ReadEnvironment(ctx)
	if err != nil {
		return state.EnvironmentState{}, state.Revision{}, newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境状态不可读", map[string]any{"field": "environment", "reason": "read_failed"}, err)
	}
	if err := validateEnvironmentReady(environment); err != nil {
		return state.EnvironmentState{}, state.Revision{}, err
	}
	revision, err := s.checkRepository(ctx, environment)
	if err != nil {
		return state.EnvironmentState{}, state.Revision{}, err
	}
	if err := s.deps.UV.Check(ctx, uv.RunOptions{
		Stage:         protocol.StageBackendSpawn,
		ProjectDir:    s.layout.RepoDir(),
		ProjectEnvDir: s.layout.VenvDir(),
	}); err != nil {
		return state.EnvironmentState{}, state.Revision{}, mapDependencyError(protocol.StageBackendSpawn, protocol.CodeEnvironmentBroken, "受管 uv 校验失败", err)
	}
	return environment, revision, nil
}

func (s *ManagedSupervisor) drainUntilTerminal(ctx context.Context, request Request, snapshot *controlState, results <-chan controlResult) (protocol.ControlCommand, error) {
	drainCtx := ctx
	var cancel context.CancelFunc
	if _, ok := terminalCommand(request.Control); ok {
		// 已接受终止命令的控制结果必须完成 FIFO 收口，不能被外层操作取消
		// 随机截断；无终止 latch 时仍遵循调用方 ctx 的取消语义。
		drainCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), controlDrainTimeout)
		defer cancel()
	}
	for {
		stage := controlSnapshotStage(snapshot, protocol.StageBackendRun)
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			return protocol.ControlCommand{}, newError(protocol.CodeInternalError, stage, "stdin 控制通道读取失败", nil, infraErr)
		}
		select {
		case <-drainCtx.Done():
			return protocol.ControlCommand{}, drainCtx.Err()
		case result := <-results:
			if result.err != nil {
				return protocol.ControlCommand{}, newError(protocol.CodeInternalError, stage, "stdin 控制通道读取失败", nil, result.err)
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					return protocol.ControlCommand{}, err
				}
			case protocol.ControlCancel, protocol.ControlShutdown:
				return result.command, nil
			}
		}
	}
}

func (s *ManagedSupervisor) finishLatchedControl(ctx context.Context, request Request, snapshot *controlState, attempt *controlAttempt, results <-chan controlResult) (bool, error) {
	if _, ok := terminalCommand(request.Control); !ok {
		return false, nil
	}
	command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
	if err != nil {
		return true, err
	}
	if attempt == nil {
		if command.Command == protocol.ControlCancel {
			cancelled := newControlCancelled(command.CommandID)
			return true, errors.Join(s.closeControlBeforeCancel(request, snapshot), cancelled)
		}
		return true, s.finishControlShutdownInterrupt(ctx, request, snapshot, command.CommandID)
	}
	if command.Command == protocol.ControlCancel {
		return true, s.finishControlCancel(ctx, request, snapshot, attempt, command.CommandID)
	}
	return true, s.finishControlShutdown(ctx, request, attempt, snapshot, command.CommandID)
}

func (s *ManagedSupervisor) finishControlShutdownInterrupt(ctx context.Context, request Request, snapshot *controlState, commandID string) error {
	if snapshot == nil {
		return s.closeControlBeforeShutdown(request, snapshot, commandID)
	}
	_, status, _ := snapshot.snapshot()
	if status == protocol.StateReadyToStart {
		return s.closeControlBeforeShutdown(request, snapshot, commandID)
	}
	return s.finishControlShutdown(ctx, request, nil, snapshot, commandID)
}

func (s *ManagedSupervisor) resolveStartAttemptError(ctx context.Context, request Request, snapshot *controlState, err error, restarting bool, restartFacts map[string]any, results <-chan controlResult) (bool, error) {
	if err == nil {
		return false, nil
	}
	stage := protocol.StageBackendSpawn
	if restarting {
		stage = protocol.StageBackendRestart
	}
	if backendErrorHasCode(err, protocol.CodeOutputWriteFailed) {
		if restarting || backendErrorCommitted(err) {
			return true, s.emitControlFailure(request, snapshot, err)
		}
		closeErr := s.closeControlBeforeFailure(request, snapshot, true)
		return true, errors.Join(closeErr, err)
	}
	if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
		failure := newError(protocol.CodeInternalError, stage, "stdin 控制通道读取失败", nil, infraErr)
		joined := errors.Join(failure, err)
		if restarting || backendErrorCommitted(err) {
			return true, s.emitControlFailure(request, snapshot, joined)
		}
		closeErr := s.closeControlBeforeFailure(request, snapshot, false)
		return true, errors.Join(closeErr, joined)
	}
	if backendErrorCommitted(err) {
		if restarting {
			return true, s.emitControlFailure(request, snapshot, newError(protocol.CodeBackendRestartFailed, protocol.StageBackendRestart, "后端重启失败", restartFailureDetails(restartFacts, err), err))
		}
		return true, s.emitControlFailure(request, snapshot, err)
	}
	if handled, terminalErr := s.finishLatchedControl(ctx, request, snapshot, nil, results); handled {
		return true, terminalErr
	}
	if ctx.Err() != nil {
		if restarting {
			return true, s.finishControlCancel(ctx, request, snapshot, nil, "")
		}
		closeErr := s.closeControlBeforeFailure(request, snapshot, false)
		return true, errors.Join(closeErr, s.finishControlCancel(ctx, request, snapshot, nil, ""))
	}
	return false, nil
}

func controlSnapshotStage(snapshot *controlState, fallback protocol.Stage) protocol.Stage {
	if snapshot == nil {
		return fallback
	}
	stage, _, _ := snapshot.snapshot()
	if stage == "" {
		return fallback
	}
	return stage
}

func (s *ManagedSupervisor) removeBackendTransaction(ctx context.Context, tx TransactionHandle) error {
	if tx == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	err := s.deps.State.RemoveBackendTransaction(cleanupCtx, tx)
	cancel()
	return mapStateCleanupError(err)
}

func (s *ManagedSupervisor) startControlAttempt(ctx context.Context, request Request, environment state.EnvironmentState, revision state.Revision, snapshot *controlState, restarting bool, results <-chan controlResult) (*controlAttempt, error) {
	if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
		return nil, newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr)
	}
	if _, ok := terminalCommand(request.Control); ok {
		command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
		if err != nil {
			return nil, err
		}
		return nil, &controlInterruptError{command: command}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	mode := modeForRequest(request)
	projectDir := s.layout.RepoDir()
	projectEnvDir := ""
	var identity *uv.SupervisionIdentity
	pythonPaths := append([]string(nil), s.deps.PythonPaths...)
	if mode == ModeDevelopment {
		projectDir = request.DevelopmentRepo
		projectEnvDir = developmentProjectEnv(projectDir)
		pythonPaths = []string{developmentPythonPath(projectDir)}
	} else {
		identity = &uv.SupervisionIdentity{Version: revision.Version, Commit: revision.Commit}
	}
	logger, err := s.deps.Logger(ctx, request)
	if err != nil || logger == nil {
		if logger != nil {
			err = errors.Join(err, mapLoggerCleanupError(logger.Close()))
		}
		if err == nil {
			err = errors.New("backend logger is nil")
		}
		return nil, withFailureDetails(newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "后端日志初始化失败", map[string]any{"sink": "runtime_log"}, err), logger, nil)
	}
	tx, err := s.deps.State.BeginBackendTransaction(ctx, TransactionInput{OperationID: request.OperationID, PID: request.RuntimePID, Version: revision.Version, Stage: protocol.StageBackendSpawn})
	if err != nil || tx == nil {
		var cleanupErr error
		if tx != nil {
			cleanupErr = errors.Join(cleanupErr, s.removeBackendTransaction(ctx, tx))
		}
		cleanupErr = errors.Join(cleanupErr, mapLoggerCleanupError(logger.Close()))
		if err == nil {
			err = errors.New("backend transaction handle is nil")
		}
		return nil, errors.Join(newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端事务写入失败", nil, err), cleanupErr)
	}
	if mode == ModeManaged {
		if err := s.recheckRevision(ctx, environment, revision); err != nil {
			cleanupErr := errors.Join(
				s.removeBackendTransaction(ctx, tx),
				mapLoggerCleanupError(logger.Close()),
			)
			return nil, errors.Join(err, cleanupErr)
		}
	}
	gate := &streamGate{stage: protocol.StageBackendSpawn}
	attempt := &controlAttempt{tx: tx, logger: logger, gate: gate, stage: protocol.StageBackendSpawn, results: results}
	if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
		cleanupErr := errors.Join(
			s.removeBackendTransaction(ctx, tx),
			mapLoggerCleanupError(logger.Close()),
		)
		return nil, errors.Join(newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "stdin 控制通道读取失败", nil, infraErr), cleanupErr)
	}
	if _, ok := terminalCommand(request.Control); ok {
		command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
		cleanupErr := func() error {
			return errors.Join(
				s.removeBackendTransaction(ctx, tx),
				mapLoggerCleanupError(logger.Close()),
			)
		}
		if drainErr != nil {
			return nil, errors.Join(drainErr, cleanupErr())
		}
		if err := cleanupErr(); err != nil {
			return nil, err
		}
		return nil, &controlInterruptError{command: command}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		cleanupErr := errors.Join(
			s.removeBackendTransaction(ctx, tx),
			mapLoggerCleanupError(logger.Close()),
		)
		return nil, errors.Join(ctxErr, cleanupErr)
	}
	proc, err := s.deps.UV.StartManaged(ctx, []string{"run", "--project", projectDir, "--no-sync", "main.py"}, uv.ManagedOptions{
		RunOptions: uv.RunOptions{Stage: protocol.StageBackendSpawn, ProjectDir: projectDir, ProjectEnvDir: projectEnvDir},
		Identity:   identity,
	}, s.streamSink(request, logger, gate))
	if err != nil || proc == nil {
		fault := gate.Fault()
		if proc != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			primary := newError(protocol.CodeBackendSpawnFailed, protocol.StageBackendSpawn, "后端进程启动失败", nil, err)
			if fault != nil {
				primary = withFailureDetailsExtra(fault, logger, proc, cleanup.details)
			}
			failure := withFailureDetailsExtra(primary, logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		cleanupErr := errors.Join(
			s.removeBackendTransaction(ctx, tx),
			mapLoggerCleanupError(logger.Close()),
		)
		if fault != nil {
			return nil, errors.Join(fault, cleanupErr)
		}
		if err == nil {
			err = errors.New("managed process is nil")
		}
		return nil, errors.Join(newError(protocol.CodeBackendSpawnFailed, protocol.StageBackendSpawn, "后端进程启动失败", nil, err), cleanupErr)
	}
	attempt.process = proc
	if fault := gate.Fault(); fault != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if !restarting {
		setControlStage(request.Control, protocol.StageBackendSpawn)
		if err := s.emitState(request.Emitter, protocol.StageBackendSpawn, protocol.StateStartingBackend, "正在启动后端", map[string]any{}); err != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(err, logger, proc, cleanup.details)))
		}
	} else {
		setControlStage(request.Control, protocol.StageBackendRestart)
		gate.SetStage(protocol.StageBackendRestart)
		attempt.stage = protocol.StageBackendRestart
	}
	if err := gate.Open(request.Emitter); err != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(err, logger, proc, cleanup.details)))
	}
	if err := s.deps.State.UpdateBackendTransaction(ctx, tx, protocol.StageBackendHealth); err != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		if fault := gate.Fault(); fault != nil {
			failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			failure := withFailureDetailsExtra(newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr), logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if _, ok := terminalCommand(request.Control); ok {
			command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
			if drainErr != nil {
				return nil, markCommitted(errors.Join(cleanup.err, drainErr))
			}
			if cleanup.err != nil {
				return nil, markCommitted(cleanup.err)
			}
			return nil, &controlInterruptError{command: command}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if cleanup.err != nil {
				return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(ctxErr, logger, proc, cleanup.details)))
			}
			return nil, ctxErr
		}
		failure := withFailureDetailsExtra(newError(protocol.CodeStateWriteFailed, protocol.StageBackendHealth, "后端事务写入失败", nil, err), logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	probe := processProbe{process: proc, uvPath: s.deps.UVPath, pythonPaths: pythonPaths}
	if restarting {
		setControlStage(request.Control, protocol.StageBackendRestart)
		snapshot.set(protocol.StageBackendRestart, protocol.StateRestarting, map[string]any{"pid": proc.PID(), "logPath": logger.LogPath()})
	} else {
		gate.SetStage(protocol.StageBackendHealth)
		setControlStage(request.Control, protocol.StageBackendHealth)
		snapshot.set(protocol.StageBackendHealth, protocol.StateStartingBackend, map[string]any{"pid": proc.PID(), "logPath": logger.LogPath()})
	}
	healthErr := s.awaitHealth(ctx, request, revision, probe, gate, snapshot, results)
	if err := healthErr; err != nil {
		var interrupted *controlInterruptError
		if errors.As(err, &interrupted) {
			if interrupted.command.Command != "" {
				interrupted.attempt = attempt
				return nil, interrupted
			}
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(interrupted.cause, logger, proc, cleanup.details)))
		}
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		if backendErrorHasCode(err, protocol.CodeOutputWriteFailed) {
			failure := withFailureDetailsExtra(err, logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if fault := gate.Fault(); fault != nil {
			failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if _, ok := terminalCommand(request.Control); ok {
			command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
			if drainErr != nil {
				return nil, markCommitted(errors.Join(cleanup.err, drainErr))
			}
			if cleanup.err != nil {
				return nil, markCommitted(cleanup.err)
			}
			return nil, &controlInterruptError{command: command}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(ctxErr, logger, proc, cleanup.details)))
		}
		failure := withFailureDetailsExtra(mapDependencyError(protocol.StageBackendHealth, protocol.CodeBackendHealthInvalid, "后端健康检查失败", err), logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if fault := gate.Fault(); fault != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if _, ok := terminalCommand(request.Control); ok {
		command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
		if drainErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			return nil, markCommitted(errors.Join(cleanup.err, drainErr))
		}
		return nil, &controlInterruptError{command: command, attempt: attempt}
	}
	if err := s.deps.State.UpdateBackendTransaction(ctx, tx, protocol.StageBackendRun); err != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		if fault := gate.Fault(); fault != nil {
			failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			failure := withFailureDetailsExtra(newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr), logger, proc, cleanup.details)
			return nil, markCommitted(errors.Join(cleanup.err, failure))
		}
		if _, ok := terminalCommand(request.Control); ok {
			command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
			if drainErr != nil {
				return nil, markCommitted(errors.Join(cleanup.err, drainErr))
			}
			if cleanup.err != nil {
				return nil, markCommitted(cleanup.err)
			}
			return nil, &controlInterruptError{command: command}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if cleanup.err != nil {
				return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(ctxErr, logger, proc, cleanup.details)))
			}
			return nil, ctxErr
		}
		failure := withFailureDetailsExtra(newError(protocol.CodeStateWriteFailed, protocol.StageBackendRun, "后端事务写入失败", nil, err), logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if fault := gate.Fault(); fault != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		failure := withFailureDetailsExtra(fault, logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		failure := withFailureDetailsExtra(newError(protocol.CodeInternalError, protocol.StageBackendRun, "stdin 控制通道读取失败", nil, infraErr), logger, proc, cleanup.details)
		return nil, markCommitted(errors.Join(cleanup.err, failure))
	}
	if _, ok := terminalCommand(request.Control); ok {
		command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
		if drainErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			return nil, markCommitted(errors.Join(cleanup.err, drainErr))
		}
		return nil, &controlInterruptError{command: command, attempt: attempt}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		return nil, markCommitted(errors.Join(cleanup.err, ctxErr))
	}
	gate.SetStage(protocol.StageBackendRun)
	setControlStage(request.Control, protocol.StageBackendRun)
	details := map[string]any{"pid": proc.PID(), "baseUrl": "http://127.0.0.1:36163", "logPath": logger.LogPath()}
	if err := s.emitState(request.Emitter, protocol.StageBackendRun, protocol.StateRunning, "后端已就绪", details); err != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		return nil, markCommitted(errors.Join(cleanup.err, withFailureDetailsExtra(err, logger, proc, cleanup.details)))
	}
	snapshot.set(protocol.StageBackendRun, protocol.StateRunning, details)
	return attempt, nil
}

func (s *ManagedSupervisor) awaitHealth(ctx context.Context, request Request, revision state.Revision, probe health.Probe, gate *streamGate, snapshot *controlState, results <-chan controlResult) error {
	healthCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	expectation := health.Expectation{Mode: health.ModeManaged, Protocol: protocol.Version, Version: revision.Version, Commit: revision.Commit}
	if modeForRequest(request) == ModeDevelopment {
		expectation.Mode = health.ModeDevelopment
		expectation.Version = ""
		expectation.Commit = ""
	}
	done := make(chan error, 1)
	go func() {
		done <- s.deps.Health.Check(healthCtx, expectation, probe)
	}()
	for {
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			cancel()
			<-done
			return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
		}
		select {
		case err := <-done:
			if fault := gate.Fault(); fault != nil {
				return fault
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
				if drainErr != nil {
					return drainErr
				}
				return &controlInterruptError{command: command}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		case <-ctx.Done():
			cancel()
			<-done
			if fault := gate.Fault(); fault != nil {
				return fault
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
				if drainErr != nil {
					return drainErr
				}
				return &controlInterruptError{command: command}
			}
			return ctx.Err()
		case <-gate.Faulted():
			cancel()
			<-done
			if fault := gate.Fault(); fault != nil {
				return fault
			}
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, drainErr := s.drainUntilTerminal(ctx, request, snapshot, results)
				if drainErr != nil {
					return drainErr
				}
				return &controlInterruptError{command: command}
			}
			return nil
		case result := <-results:
			if result.err != nil {
				cancel()
				<-done
				if fault := gate.Fault(); fault != nil {
					return fault
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
				}
				return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, result.err)}
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					cancel()
					<-done
					return err
				}
			case protocol.ControlCancel, protocol.ControlShutdown:
				cancel()
				err := <-done
				if fault := gate.Fault(); fault != nil {
					return fault
				}
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return &controlInterruptError{cause: newError(protocol.CodeInternalError, protocol.StageBackendHealth, "stdin 控制通道读取失败", nil, infraErr)}
				}
				return &controlInterruptError{command: result.command, cause: err}
			}
		}
	}
}

func startControlForwarder(ctx context.Context, receiver ControlReceiver) (<-chan controlResult, <-chan struct{}) {
	if receiver == nil {
		return nil, nil
	}
	results := make(chan controlResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			command, err := receiver.Receive(ctx)
			if err != nil {
				if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					if source, ok := receiver.(interface{ InfrastructureError() error }); !ok || source.InfrastructureError() == nil {
						return
					}
				}
				select {
				case results <- controlResult{command: command, err: err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case results <- controlResult{command: command, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return results, done
}

func terminalCommand(receiver ControlReceiver) (protocol.ControlCommand, bool) {
	if receiver == nil {
		return protocol.ControlCommand{}, false
	}
	if source, ok := receiver.(interface {
		TerminalCommand() (protocol.ControlCommand, bool)
	}); ok {
		return source.TerminalCommand()
	}
	return protocol.ControlCommand{}, false
}

func setControlStage(receiver ControlReceiver, stage protocol.Stage) {
	if setter, ok := receiver.(interface{ SetStage(protocol.Stage) }); ok {
		setter.SetStage(stage)
	}
}

func controlInfrastructureError(receiver ControlReceiver) error {
	if source, ok := receiver.(interface{ InfrastructureError() error }); ok {
		return source.InfrastructureError()
	}
	return nil
}

func (s *ManagedSupervisor) waitRestart(ctx context.Context, request Request, snapshot *controlState, results <-chan controlResult) error {
	delay := s.deps.RestartDelay
	if delay <= 0 {
		return nil
	}
	var timer Timer
	switch {
	case s.deps.NewTimer != nil:
		timer = s.deps.NewTimer(delay)
	case s.deps.Timer != nil:
		timer = channelTimer{channel: s.deps.Timer(delay)}
	default:
		timer = realTimer{timer: time.NewTimer(delay)}
	}
	if timer == nil {
		return errors.New("backend restart timer is nil")
	}
	defer timer.Stop()
	for {
		if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
			return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, infraErr)
		}
		// Timer 与 terminal 同时 ready 时，先按 mailbox/FIFO 顺序收口，
		// 不允许随机选择 timer 而跳过已接受的终止命令。
		if _, ok := terminalCommand(request.Control); ok {
			command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
			if err != nil {
				return err
			}
			return &controlInterruptError{command: command}
		}
		select {
		case <-ctx.Done():
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, infraErr)
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
				if err != nil {
					return err
				}
				return &controlInterruptError{command: command}
			}
			return ctx.Err()
		case <-timer.C():
			if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
				return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, infraErr)
			}
			if _, ok := terminalCommand(request.Control); ok {
				command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
				if err != nil {
					return err
				}
				return &controlInterruptError{command: command}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return nil
		case result := <-results:
			if result.err != nil {
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, infraErr)
				}
				if _, ok := terminalCommand(request.Control); ok {
					command, err := s.drainUntilTerminal(ctx, request, snapshot, results)
					if err != nil {
						return err
					}
					return &controlInterruptError{command: command}
				}
				return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, result.err)
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, details := snapshot.snapshot()
				details = protocol.WithControlCommandID(details, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", details); err != nil {
					return err
				}
			case protocol.ControlCancel, protocol.ControlShutdown:
				if infraErr := controlInfrastructureError(request.Control); infraErr != nil {
					return newError(protocol.CodeInternalError, protocol.StageBackendRestart, "stdin 控制通道读取失败", nil, infraErr)
				}
				return &controlInterruptError{command: result.command}
			}
		}
	}
}

type channelTimer struct{ channel <-chan time.Time }

func (t channelTimer) C() <-chan time.Time { return t.channel }
func (channelTimer) Stop() bool            { return false }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

func newControlCancelled(commandID string) error {
	details := map[string]any{}
	if commandID != "" {
		details["controlCommandId"] = commandID
	}
	return newError(protocol.CodeOperationCancelled, protocol.StageBackendShutdown, "操作已取消", details, context.Canceled)
}

func (s *ManagedSupervisor) finishControlCancel(ctx context.Context, request Request, snapshot *controlState, attempt *controlAttempt, commandID string) error {
	details := map[string]any{}
	if commandID != "" {
		details["controlCommandId"] = commandID
	}
	primary := newControlCancelled(commandID)
	if attempt == nil {
		if controlErr := s.closeControlBeforeCancel(request, snapshot); controlErr != nil {
			return errors.Join(controlErr, primary)
		}
	}
	if attempt == nil && snapshot != nil {
		_, status, _ := snapshot.snapshot()
		if status == protocol.StateReadyToStart {
			return primary
		}
	}
	setControlStage(request.Control, protocol.StageBackendShutdown)
	if snapshot != nil {
		snapshot.set(protocol.StageBackendShutdown, protocol.StateStoppingBackend, details)
	}
	stateErr := s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStoppingBackend, "正在关闭后端", details)
	if attempt == nil || attempt.process == nil {
		if stateErr != nil {
			return errors.Join(stateErr, s.emitControlFailure(request, snapshot, errors.Join(stateErr, primary)))
		}
		if resourceErr := snapshot.finalizeResources(); resourceErr != nil {
			return errors.Join(stateErr, s.emitControlFailure(request, snapshot, errors.Join(resourceErr, primary)))
		}
		if snapshot != nil {
			snapshot.set(protocol.StageBackendShutdown, protocol.StateStopped, details)
		}
		stoppedErr := s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStopped, "后端已停止", details)
		return errors.Join(stateErr, stoppedErr, primary)
	}

	cleanupDone := make(chan processCleanup, 1)
	go func() {
		cleanupDone <- s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
	}()
	var controlErr error
	if stateErr != nil {
		controlErr = stateErr
	}
	controlEnded := request.BeforeControlClose == nil
	var closeControlOnce sync.Once
	closeControl := func() {
		closeControlOnce.Do(func() {
			if request.BeforeControlClose != nil {
				request.BeforeControlClose()
			}
			snapshot.markControlClosed()
		})
	}
	var cleanupResult *processCleanup
	finalize := func(cleanup processCleanup) error {
		if controlErr != nil {
			return errors.Join(stateErr, s.emitControlFailure(request, snapshot, errors.Join(cleanup.err, controlErr, primary)))
		}
		if cleanup.err != nil {
			return errors.Join(stateErr, s.emitControlFailure(request, snapshot, errors.Join(cleanup.err, primary)))
		}
		if resourceErr := snapshot.finalizeResources(); resourceErr != nil {
			return errors.Join(stateErr, s.emitControlFailure(request, snapshot, errors.Join(resourceErr, primary)))
		}
		if snapshot != nil {
			snapshot.set(protocol.StageBackendShutdown, protocol.StateStopped, details)
		}
		stoppedErr := s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStopped, "后端已停止", details)
		return errors.Join(stateErr, stoppedErr, primary)
	}
	for {
		select {
		case cleanup := <-cleanupDone:
			cleanupResult = &cleanup
			if controlEnded {
				closeControl()
				return finalize(cleanup)
			}
			closeControl()
		case result := <-attempt.results:
			if result.err != nil {
				controlEnded = true
				if !errors.Is(result.err, ErrControlStopped) && !errors.Is(result.err, ErrControlMailboxClosed) {
					controlErr = newError(protocol.CodeInternalError, protocol.StageBackendCleanup, "stdin 控制通道读取失败", nil, result.err)
				}
				if cleanupResult != nil {
					closeControl()
					return finalize(*cleanupResult)
				}
				continue
			}
			switch result.command.Command {
			case protocol.ControlStatus:
				stage, status, statusDetails := snapshot.snapshot()
				statusDetails = protocol.WithControlCommandID(statusDetails, result.command.CommandID)
				if err := s.emitState(request.Emitter, stage, status, "后端状态快照", statusDetails); err != nil {
					controlErr = err
				}
			case protocol.ControlCancel, protocol.ControlShutdown:
				// 首个终止命令已经决定终态；后续终止命令只保留 FIFO 顺序而忽略副作用。
			}
		}
	}
}

func (s *ManagedSupervisor) finishControlShutdown(ctx context.Context, request Request, attempt *controlAttempt, snapshot *controlState, commandID string) error {
	details := map[string]any{}
	if commandID != "" {
		details["controlCommandId"] = commandID
	}
	controlClosed := false
	if attempt == nil {
		if err := s.closeControlBeforeShutdown(request, snapshot, commandID); err != nil {
			return err
		}
		controlClosed = true
	}
	if !controlClosed && request.BeforeShutdown != nil {
		request.BeforeShutdown(commandID)
	}
	setControlStage(request.Control, protocol.StageBackendShutdown)
	snapshot.set(protocol.StageBackendShutdown, protocol.StateStoppingBackend, details)
	if err := s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStoppingBackend, "正在关闭后端", details); err != nil {
		var cleanup processCleanup
		if attempt != nil && attempt.process != nil {
			cleanup = s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
		}
		return errors.Join(cleanup.err, err)
	}
	if attempt == nil || attempt.process == nil {
		if resourceErr := snapshot.finalizeResources(); resourceErr != nil {
			return s.emitControlFailure(request, snapshot, resourceErr)
		}
		snapshot.set(protocol.StageBackendShutdown, protocol.StateStopped, details)
		return s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStopped, "后端已停止", details)
	}
	timeout := s.deps.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	closer := s.deps.HTTP
	if closer == nil {
		closer = fixedHTTPCloser{}
	}
	httpErr := closer.Close(closeCtx)
	graceful := httpErr == nil && waitProcessExit(closeCtx, attempt.process)
	if httpErr == nil && graceful {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
		if cleanup.err != nil {
			return s.emitControlFailure(request, snapshot, cleanup.err)
		}
		if resourceErr := snapshot.finalizeResources(); resourceErr != nil {
			return s.emitControlFailure(request, snapshot, resourceErr)
		}
		if cleanup.forced {
			if err := emitForceWarning(request.Emitter, cleanup.details); err != nil {
				return err
			}
		}
		snapshot.set(protocol.StageBackendShutdown, protocol.StateStopped, details)
		return s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStopped, "后端已停止", details)
	}
	cleanup := s.cleanupProcess(context.WithoutCancel(ctx), attempt.process, attempt.tx, attempt.logger)
	if cleanup.err != nil {
		failure := newError(protocol.CodeBackendShutdownFailed, protocol.StageBackendCleanup, "后端进程树未能确认清空", cleanup.details, httpErr)
		return s.emitControlFailure(request, snapshot, errors.Join(failure, cleanup.err))
	}
	if resourceErr := snapshot.finalizeResources(); resourceErr != nil {
		return s.emitControlFailure(request, snapshot, resourceErr)
	}
	if err := emitForceWarning(request.Emitter, cleanup.details); err != nil {
		return err
	}
	snapshot.set(protocol.StageBackendShutdown, protocol.StateStopped, details)
	return s.emitState(request.Emitter, protocol.StageBackendShutdown, protocol.StateStopped, "后端已停止", details)
}

func emitForceWarning(emitter EventEmitter, details map[string]any) error {
	warning, err := protocol.NewWarningEvent(protocol.CodeBackendForceTerminated, protocol.StageBackendShutdown, "后端已强制终止", details)
	if err != nil {
		return err
	}
	if err := emitter.EmitWarning(warning); err != nil {
		return newError(protocol.CodeOutputWriteFailed, protocol.StageBackendShutdown, "协议 warning 输出失败", nil, err)
	}
	return nil
}

func waitProcessExit(ctx context.Context, proc ManagedProcess) bool {
	if proc == nil {
		return false
	}
	select {
	case <-proc.Exited():
		return true
	case <-ctx.Done():
		return false
	}
}

type fixedHTTPCloser struct{}

func (fixedHTTPCloser) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("backend shutdown context is nil")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, backendCloseURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	readErr := error(nil)
	if response.Body != nil {
		_, readErr = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	}
	closeErr := error(nil)
	if response.Body != nil {
		closeErr = response.Body.Close()
	}
	client.CloseIdleConnections()
	statusErr := error(nil)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		statusErr = fmt.Errorf("backend close returned status %d", response.StatusCode)
	}
	return errors.Join(statusErr, readErr, closeErr)
}

var _ HTTPCloser = fixedHTTPCloser{}
