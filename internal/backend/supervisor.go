package backend

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

const cleanupTimeout = 30 * time.Second

// ManagedSupervisor 监督单个受管后端 Job 的完整生命周期。
type ManagedSupervisor struct {
	layout *config.Layout
	deps   Dependencies
}

// NewManagedSupervisor 创建可按请求选择 managed 或 development 的后端监督器。
func NewManagedSupervisor(layout *config.Layout, deps Dependencies) (*ManagedSupervisor, error) {
	if layout == nil {
		return nil, errors.New("backend layout is nil")
	}
	if deps.Lock == nil || deps.State == nil || deps.Repository == nil ||
		deps.Entry == nil || deps.UV == nil || deps.Health == nil || deps.Logger == nil {
		return nil, errors.New("backend dependencies are incomplete")
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.ShutdownTimeout <= 0 {
		deps.ShutdownTimeout = defaultShutdownTimeout
	}
	if deps.RestartDelay <= 0 {
		deps.RestartDelay = defaultRestartDelay
	}
	if deps.UVPath == "" {
		deps.UVPath = deps.UV.Executable()
	}
	if deps.PythonPath == "" {
		deps.PythonPath = layout.VenvPythonExecutable()
	}
	if len(deps.PythonPaths) == 0 && deps.PythonPath != "" {
		deps.PythonPaths = []string{deps.PythonPath}
	}
	if deps.UVPath == "" || deps.PythonPath == "" {
		return nil, errors.New("backend process identity paths are incomplete")
	}
	return &ManagedSupervisor{layout: layout, deps: deps}, nil
}

// Supervise 启动并长驻监督指定模式的后端，直到调用方取消或 Job 根进程退出。
func (s *ManagedSupervisor) Supervise(ctx context.Context, request Request) (returnErr error) {
	if ctx == nil {
		return newError(protocol.CodeInvalidArgument, protocol.StageBackendSpawn, "后端监督上下文不可用", nil, errors.New("backend context is nil"))
	}
	if s == nil || s.layout == nil {
		return newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "后端监督器不可用", nil, errors.New("backend supervisor is nil"))
	}
	if request.OperationID == "" || request.RuntimePID == 0 || request.Emitter == nil {
		return newError(protocol.CodeInvalidArgument, protocol.StageBackendSpawn, "后端监督请求无效", nil, errors.New("backend request is invalid"))
	}
	mode := modeForRequest(request)
	if mode != ModeManaged && mode != ModeDevelopment {
		return newError(protocol.CodeUnsupportedMode, protocol.StageBackendSpawn, "后端运行模式不受支持", map[string]any{"mode": string(mode)}, errors.New("backend mode is unsupported"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode == ModeDevelopment {
		var err error
		request, err = s.normalizeDevelopmentRequest(ctx, request)
		if err != nil {
			return err
		}
		if request.Control == nil {
			mailbox := NewControlMailbox(defaultControlMailboxCapacity)
			request.Control = mailbox
			defer mailbox.Close()
		}
	}
	if request.Control != nil {
		return s.superviseControlled(ctx, request)
	}

	locks, err := s.deps.Lock.Acquire(ctx)
	if err != nil || locks == nil {
		if err == nil {
			err = errors.New("backend lock lease is nil")
		}
		primary := preferCancellation(ctx, mapDependencyError(protocol.StageBackendSpawn, protocol.CodeMutexOperationFailed, "后端 Mutex 获取失败", err))
		var cleanupErr error
		if locks != nil {
			cleanupErr = errors.Join(cleanupErr, mapMutexCleanupError(locks.Close()))
		}
		cleanupErr = errors.Join(cleanupErr, mapStateCleanupError(s.deps.State.Close()))
		cleanupErr = errors.Join(cleanupErr, mapMutexCleanupError(s.deps.Lock.Close()))
		return errors.Join(primary, cleanupErr)
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			returnErr = errors.Join(returnErr, mapMutexCleanupError(locks.Close()))
			returnErr = errors.Join(returnErr, mapMutexCleanupError(s.deps.Lock.Close()))
		}
	}()
	stateOwned := true
	defer func() {
		if stateOwned {
			returnErr = errors.Join(returnErr, mapStateCleanupError(s.deps.State.Close()))
		}
	}()

	if err := s.recoverStaleTransaction(ctx); err != nil {
		return preferCancellation(ctx, err)
	}
	if err := s.checkEntry(ctx); err != nil {
		return preferCancellation(ctx, err)
	}
	environment, err := s.deps.State.ReadEnvironment(ctx)
	if err != nil {
		readErr := newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境状态不可读", map[string]any{
			"field":  "environment",
			"reason": "read_failed",
		}, err)
		return preferCancellation(ctx, readErr)
	}
	if err := validateEnvironmentReady(environment); err != nil {
		return preferCancellation(ctx, err)
	}
	revision, err := s.checkRepository(ctx, environment)
	if err != nil {
		return preferCancellation(ctx, err)
	}
	if err := s.deps.UV.Check(ctx); err != nil {
		return preferCancellation(ctx, mapDependencyError(protocol.StageBackendSpawn, protocol.CodeUVExecFailed, "受管 uv 校验失败", err))
	}

	logger, err := s.deps.Logger(ctx, request)
	if err != nil || logger == nil {
		if logger != nil {
			err = errors.Join(err, mapLoggerCleanupError(logger.Close()))
		}
		if err == nil {
			err = errors.New("backend logger is nil")
		}
		failure := withFailureDetails(newError(protocol.CodeInternalError, protocol.StageBackendSpawn, "后端日志初始化失败", map[string]any{"sink": "runtime_log"}, err), logger, nil)
		return preferCancellation(ctx, failure)
	}
	loggerOwned := true
	defer func() {
		if loggerOwned {
			returnErr = errors.Join(returnErr, mapLoggerCleanupError(logger.Close()))
		}
	}()

	tx, err := s.deps.State.BeginBackendTransaction(ctx, TransactionInput{
		OperationID: request.OperationID,
		PID:         request.RuntimePID,
		Version:     revision.Version,
		Stage:       protocol.StageBackendSpawn,
	})
	txOwned := tx != nil
	defer func() {
		if txOwned {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			removeErr := s.deps.State.RemoveBackendTransaction(cleanupCtx, tx)
			cancel()
			returnErr = errors.Join(returnErr, mapStateCleanupError(removeErr))
		}
	}()
	if err != nil || tx == nil {
		if err == nil {
			err = errors.New("backend transaction handle is nil")
		}
		failure := withFailureDetails(newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端事务写入失败", nil, err), logger, nil)
		return preferCancellation(ctx, failure)
	}

	// 事务建立后再次读取环境与 revision，避免检查与 spawn 之间使用过期事实。
	if err := s.recheckRevision(ctx, environment, revision); err != nil {
		return preferCancellation(ctx, withFailureDetails(err, logger, nil))
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	gate := &streamGate{stage: protocol.StageBackendSpawn}
	sink := s.streamSink(request, logger, gate)
	processOwned := false
	proc, err := s.deps.UV.StartManaged(ctx, []string{
		"run", "--project", s.layout.RepoDir(), "--no-sync", "main.py",
	}, uv.ManagedOptions{
		RunOptions: uv.RunOptions{
			Stage:      protocol.StageBackendSpawn,
			ProjectDir: s.layout.RepoDir(),
			Line:       nil,
		},
		Identity: &uv.SupervisionIdentity{Version: revision.Version, Commit: revision.Commit},
	}, sink)
	if err != nil || proc == nil {
		if fault := gate.Fault(); fault != nil {
			if proc != nil {
				cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
				processOwned, txOwned, loggerOwned = false, false, false
				return errors.Join(cleanup.err, withFailureDetailsExtra(fault, logger, proc, cleanup.details))
			}
			return withFailureDetails(fault, logger, nil)
		}
		if err == nil {
			err = errors.New("managed process is nil")
		}
		spawnFailure := preferCancellation(ctx, newError(protocol.CodeBackendSpawnFailed, protocol.StageBackendSpawn, "后端进程启动失败", nil, err))
		if proc != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			processOwned, txOwned, loggerOwned = false, false, false
			primary := withFailureDetailsExtra(spawnFailure, logger, proc, cleanup.details)
			return errors.Join(cleanup.err, primary)
		}
		return withFailureDetails(spawnFailure, logger, proc)
	}
	processOwned = true
	defer func() {
		if processOwned {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, nil, nil)
			returnErr = errors.Join(returnErr, cleanup.err)
		}
	}()
	if fault := gate.Fault(); fault != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		processOwned, txOwned, loggerOwned = false, false, false
		return errors.Join(cleanup.err, withFailureDetailsExtra(fault, logger, proc, cleanup.details))
	}

	if err := s.emitState(request.Emitter, protocol.StageBackendSpawn, protocol.StateStartingBackend, "正在启动后端", map[string]any{}); err != nil {
		return err
	}
	if err := gate.Open(request.Emitter); err != nil {
		return s.failAfterStarting(request, proc, tx, logger, gate, err, &processOwned, &txOwned, &loggerOwned)
	}
	gate.SetStage(protocol.StageBackendHealth)
	if err := s.deps.State.UpdateBackendTransaction(ctx, tx, protocol.StageBackendHealth); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			processOwned, txOwned, loggerOwned = false, false, false
			return errors.Join(cleanup.err, ctxErr)
		}
		return s.failAfterStarting(request, proc, tx, logger, gate, newError(protocol.CodeStateWriteFailed, protocol.StageBackendHealth, "后端事务写入失败", nil, err), &processOwned, &txOwned, &loggerOwned)
	}
	if fault := gate.Fault(); fault != nil {
		return s.failAfterStarting(request, proc, tx, logger, gate, fault, &processOwned, &txOwned, &loggerOwned)
	}

	probe := processProbe{process: proc, uvPath: s.deps.UVPath, pythonPaths: append([]string(nil), s.deps.PythonPaths...)}
	if err := s.deps.Health.Check(ctx, health.Expectation{
		Mode:     health.ModeManaged,
		Protocol: protocol.Version,
		Version:  revision.Version,
		Commit:   revision.Commit,
	}, probe); err != nil {
		if fault := gate.Fault(); fault != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			processOwned, txOwned, loggerOwned = false, false, false
			return errors.Join(cleanup.err, withFailureDetailsExtra(fault, logger, proc, cleanup.details))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			processOwned, txOwned, loggerOwned = false, false, false
			return errors.Join(cleanup.err, ctxErr)
		}
		mapped := withFailureDetails(mapDependencyError(protocol.StageBackendHealth, protocol.CodeBackendHealthInvalid, "后端健康检查失败", err), logger, proc)
		return s.failAfterStarting(request, proc, tx, logger, gate, mapped, &processOwned, &txOwned, &loggerOwned)
	}

	if err := s.deps.State.UpdateBackendTransaction(ctx, tx, protocol.StageBackendRun); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
			processOwned, txOwned, loggerOwned = false, false, false
			return errors.Join(cleanup.err, ctxErr)
		}
		return s.failAfterStarting(request, proc, tx, logger, gate, newError(protocol.CodeStateWriteFailed, protocol.StageBackendRun, "后端事务写入失败", nil, err), &processOwned, &txOwned, &loggerOwned)
	}
	if fault := gate.Fault(); fault != nil {
		return s.failAfterStarting(request, proc, tx, logger, gate, fault, &processOwned, &txOwned, &loggerOwned)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		processOwned, txOwned, loggerOwned = false, false, false
		return errors.Join(cleanup.err, ctxErr)
	}
	gate.SetStage(protocol.StageBackendRun)
	if err := s.emitState(request.Emitter, protocol.StageBackendRun, protocol.StateRunning, "后端已就绪", map[string]any{
		"pid":     proc.PID(),
		"baseUrl": "http://127.0.0.1:36163",
		"logPath": logger.LogPath(),
	}); err != nil {
		return s.failAfterStarting(request, proc, tx, logger, gate, err, &processOwned, &txOwned, &loggerOwned)
	}

	select {
	case <-ctx.Done():
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		processOwned, txOwned, loggerOwned = false, false, false
		primary := ctx.Err()
		if fault := gate.Fault(); fault != nil {
			primary = withFailureDetailsExtra(fault, logger, proc, cleanup.details)
		}
		return errors.Join(cleanup.err, primary)
	case <-proc.Exited():
		cleanup := s.cleanupProcess(context.WithoutCancel(ctx), proc, tx, logger)
		processOwned, txOwned, loggerOwned = false, false, false
		primary := error(newCommittedError(protocol.CodeBackendExitedUnexpectedly, protocol.StageBackendRun, "后端意外退出", nil, nil))
		if fault := gate.Fault(); fault != nil {
			primary = withFailureDetails(fault, logger, proc)
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			primary = ctxErr
		}
		return errors.Join(cleanup.err, withFailureDetailsExtra(primary, logger, proc, cleanup.details))
	}
}

func (s *ManagedSupervisor) recoverStaleTransaction(ctx context.Context) error {
	tx, err := s.deps.State.ReadBackendTransaction(ctx)
	if errors.Is(err, ErrTransactionNotFound) {
		return nil
	}
	if err != nil {
		return preferCancellation(ctx, newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端事务读取失败", map[string]any{
			"field":  "backend_transaction",
			"reason": "read_failed",
		}, err))
	}
	if tx.PID == 0 || tx.Handle == nil {
		return newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端事务无效", nil, errors.New("backend transaction identity is invalid"))
	}
	if s.deps.PID != nil {
		alive, probeErr := s.deps.PID.Alive(ctx, tx.PID)
		if probeErr != nil {
			return newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端事务进程状态不可确认", nil, probeErr)
		}
		if !alive {
			if removeErr := s.deps.State.RemoveBackendTransaction(ctx, tx.Handle); removeErr != nil {
				return newError(protocol.CodeStateWriteFailed, protocol.StageBackendSpawn, "后端陈旧事务清理失败", nil, removeErr)
			}
			return nil
		}
	}
	return newError(protocol.CodeUpdateStateAmbiguous, protocol.StageBackendSpawn, "后端事务与 Mutex 状态不一致", map[string]any{
		"reason": "transaction_pid_alive_without_backend_mutex",
		"pid":    tx.PID,
	}, nil)
}

func (s *ManagedSupervisor) checkEntry(ctx context.Context) error {
	if err := s.deps.Entry.Check(ctx, s.layout.BackendEntryFile()); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, ErrEntryUnsafe) {
			return newError(protocol.CodeUnsafeReparsePoint, protocol.StageBackendSpawn, "后端入口文件身份不安全", nil, err)
		}
		return newError(protocol.CodeBackendEntryNotFound, protocol.StageBackendSpawn, "后端入口文件不存在", nil, err)
	}
	return nil
}

func validateEnvironmentReady(environment state.EnvironmentState) error {
	if environment.Status != protocol.StateReadyToStart {
		return newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境尚未就绪", environmentStateDetails(environment), nil)
	}
	if environment.LastSuccessful.Version == "" || environment.LastSuccessful.Commit == "" {
		return newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境 revision 缺失", map[string]any{"field": "lastSuccessful"}, nil)
	}
	return nil
}

func environmentStateDetails(environment state.EnvironmentState) map[string]any {
	details := map[string]any{"state": string(environment.Status)}
	if environment.Broken == nil {
		return details
	}
	details["reason"] = string(environment.Broken.Reason)
	details["brokenStage"] = string(environment.Broken.Stage)
	details["exitCode"] = environment.Broken.ExitCode
	if environment.Broken.LogPath != "" {
		details["logPath"] = environment.Broken.LogPath
	}
	return details
}

func (s *ManagedSupervisor) checkRepository(ctx context.Context, environment state.EnvironmentState) (state.Revision, error) {
	result, err := s.deps.Repository.Check(ctx)
	if err != nil {
		return state.Revision{}, preferCancellation(ctx, newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管仓库校验失败", map[string]any{
			"field":  "revision",
			"reason": "read_failed",
		}, err))
	}
	if !result.Healthy || result.Version != environment.LastSuccessful.Version || result.Commit != environment.LastSuccessful.Commit {
		details := map[string]any{"field": "revision"}
		if result.Reason != "" {
			details["reason"] = result.Reason
		}
		return state.Revision{}, newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管仓库 revision 不匹配", details, nil)
	}
	return state.Revision{Version: result.Version, Commit: result.Commit}, nil
}

func (s *ManagedSupervisor) recheckRevision(ctx context.Context, environment state.EnvironmentState, revision state.Revision) error {
	latest, err := s.deps.State.ReadEnvironment(ctx)
	if err != nil {
		return preferCancellation(ctx, newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境状态不可读", map[string]any{
			"field":  "environment",
			"reason": "read_failed",
		}, err))
	}
	if latest.Status != protocol.StateReadyToStart || latest.LastSuccessful != environment.LastSuccessful {
		details := environmentStateDetails(latest)
		details["field"] = "lastSuccessful"
		if _, ok := details["reason"]; !ok {
			details["reason"] = "changed_before_spawn"
		}
		return newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管环境在启动前发生变化", details, nil)
	}
	result, err := s.deps.Repository.Check(ctx)
	if err != nil || !result.Healthy || result.Version != revision.Version || result.Commit != revision.Commit {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, err)
		}
		details := map[string]any{"field": "revision", "reason": "changed_before_spawn"}
		if err != nil {
			details["reason"] = "read_failed"
		} else if result.Reason != "" {
			details["reason"] = result.Reason
		}
		if err == nil {
			err = errors.New("repository revision changed before spawn")
		}
		return newError(protocol.CodeEnvironmentBroken, protocol.StageBackendSpawn, "受管仓库在启动前发生变化", details, err)
	}
	return nil
}

func preferCancellation(ctx context.Context, err error) error {
	if ctx == nil || err == nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}

type streamGate struct {
	mu           sync.Mutex
	open         bool
	fault        error
	faulted      chan struct{}
	stage        protocol.Stage
	pending      []protocol.LogEvent
	pendingBytes int
}

const (
	maxPendingLogEvents = 256
	maxPendingLogBytes  = 4 << 20
)

func (g *streamGate) SetStage(stage protocol.Stage) {
	g.mu.Lock()
	g.stage = stage
	g.mu.Unlock()
}

func (g *streamGate) Fault() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fault
}

func (g *streamGate) Faulted() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.faulted == nil {
		g.faulted = make(chan struct{})
		if g.fault != nil {
			close(g.faulted)
		}
	}
	return g.faulted
}

func (g *streamGate) setFaultLocked(err error) {
	if err == nil || g.fault != nil {
		return
	}
	g.fault = err
	if g.faulted == nil {
		g.faulted = make(chan struct{})
	}
	close(g.faulted)
}

func (g *streamGate) Emit(emitter EventEmitter, event protocol.LogEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fault != nil {
		return g.fault
	}
	if !g.open {
		if len(g.pending) >= maxPendingLogEvents || g.pendingBytes+len(event.Message) > maxPendingLogBytes {
			g.setFaultLocked(newError(protocol.CodeOutputWriteFailed, g.stage, "后端日志协议输出失败", map[string]any{"sink": "protocol_output", "reason": "pending_log_overflow"}, nil))
			return g.fault
		}
		g.pending = append(g.pending, event)
		g.pendingBytes += len(event.Message)
		return nil
	}
	if err := emitter.EmitLog(event); err != nil {
		g.setFaultLocked(newError(protocol.CodeOutputWriteFailed, g.stage, "后端日志协议输出失败", map[string]any{"sink": "protocol_output"}, err))
		return g.fault
	}
	return nil
}

func (g *streamGate) Open(emitter EventEmitter) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fault != nil {
		return g.fault
	}
	for _, event := range g.pending {
		if err := emitter.EmitLog(event); err != nil {
			g.setFaultLocked(newError(protocol.CodeOutputWriteFailed, g.stage, "后端日志协议输出失败", map[string]any{"sink": "protocol_output"}, err))
			return g.fault
		}
	}
	g.pending = nil
	g.pendingBytes = 0
	g.open = true
	return nil
}

func (g *streamGate) setFault(err error) {
	if err == nil {
		return
	}
	g.mu.Lock()
	g.setFaultLocked(err)
	g.mu.Unlock()
}

func (s *ManagedSupervisor) streamSink(request Request, logger Logger, gate *streamGate) process.StreamSink {
	return func(ctx context.Context, record process.StreamRecord) error {
		if err := logger.Record(ctx, record); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
				return ctxErr
			}
			gate.mu.Lock()
			stage := gate.stage
			gate.mu.Unlock()
			fault := newError(protocol.CodeInternalError, stage, "后端运行日志写入失败", map[string]any{"sink": "runtime_log"}, err)
			gate.setFault(fault)
			return fault
		}
		// Fragment 仅写入 runtime logger；协议出口只发送最终 Event，或空行。
		if record.Event == "" && !record.EndOfLine {
			return nil
		}
		if err := gate.Emit(request.Emitter, protocol.LogEvent{
			Source:  "backend",
			Stream:  record.Stream,
			Message: record.Event,
		}); err != nil {
			return err
		}
		return nil
	}
}

func (s *ManagedSupervisor) emitState(emitter EventEmitter, stage protocol.Stage, status protocol.StateStatus, message string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	if err := emitter.EmitState(protocol.StateEvent{Stage: stage, Status: status, Message: message, Details: details}); err != nil {
		return newError(protocol.CodeOutputWriteFailed, stage, "协议状态输出失败", nil, err)
	}
	return nil
}

func (s *ManagedSupervisor) failAfterStarting(
	request Request,
	proc ManagedProcess,
	tx TransactionHandle,
	logger Logger,
	gate *streamGate,
	primary error,
	processOwned, txOwned, loggerOwned *bool,
) error {
	if fault := gate.Fault(); fault != nil {
		primary = withFailureDetails(fault, logger, proc)
	}
	primary = markCommitted(primary)
	cleanup := s.cleanupProcess(context.Background(), proc, tx, logger)
	primary = withFailureDetailsExtra(primary, logger, proc, cleanup.details)
	*processOwned, *txOwned, *loggerOwned = false, false, false
	stateErr := s.emitState(request.Emitter, protocol.StageBackendCleanup, protocol.StateBackendFailed, "后端启动失败", failureDetails(logger, proc, cleanup.details))
	return errors.Join(cleanup.err, primary, stateErr)
}

func markCommitted(err error) error {
	if err == nil {
		return nil
	}
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr == nil || backendErr.committed {
		return err
	}
	return &Error{code: backendErr.code, stage: backendErr.stage, message: backendErr.message, details: backendErr.Details(), cause: err, committed: true}
}

func withFailureDetails(err error, logger Logger, proc ManagedProcess) error {
	return withFailureDetailsExtra(err, logger, proc, nil)
}

func withFailureDetailsExtra(err error, logger Logger, proc ManagedProcess, extra map[string]any) error {
	if err == nil {
		return nil
	}
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr == nil {
		return err
	}
	details := backendErr.Details()
	for key, value := range failureDetails(logger, proc, extra) {
		details[key] = value
	}
	return &Error{code: backendErr.code, stage: backendErr.stage, message: backendErr.message, details: details, cause: err, committed: backendErr.committed}
}

func failureDetails(logger Logger, proc ManagedProcess, extra map[string]any) map[string]any {
	details := make(map[string]any, len(extra)+2)
	for key, value := range extra {
		details[key] = value
	}
	if logger != nil {
		if path := logger.LogPath(); path != "" {
			details["logPath"] = path
		}
	}
	if proc != nil {
		if pid := proc.PID(); pid != 0 {
			details["pid"] = pid
		}
	}
	return details
}

type processCleanup struct {
	details map[string]any
	err     error
	forced  bool
}

func (s *ManagedSupervisor) cleanupProcess(ctx context.Context, proc ManagedProcess, tx TransactionHandle, logger Logger) processCleanup {
	outcome := processCleanup{details: map[string]any{}}
	if ctx == nil {
		ctx = context.Background()
	}
	if proc == nil {
		outcome.err = newCommittedError(protocol.CodeInternalError, protocol.StageBackendCleanup, "后端进程资源不可用", nil, errors.New("managed process is nil"))
		return outcome
	}
	for key, value := range failureDetails(logger, proc, nil) {
		outcome.details[key] = value
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var resultErr error
	select {
	case <-proc.Exited():
		exitResult, waitErr := proc.Wait(cleanupCtx)
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			outcome.details["exitCode"] = exitResult.ExitCode
		}
		resultErr = errors.Join(resultErr, mapCleanupProcessError("wait", withoutExpectedCancellation(waitErr, cleanupCtx.Err() != nil)))
	default:
		resultErr = errors.Join(resultErr, mapCleanupProcessError("terminate", proc.Terminate(1)))
		exitResult, waitErr := proc.Wait(cleanupCtx)
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			outcome.details["exitCode"] = exitResult.ExitCode
		}
		resultErr = errors.Join(resultErr, mapCleanupProcessError("wait", withoutExpectedCancellation(waitErr, cleanupCtx.Err() != nil)))
	}
	waitEmptyErr := proc.WaitEmpty(cleanupCtx)
	if waitEmptyErr != nil {
		// 根进程可能已退出但后代仍占用 Job；先强制终止，再次 Wait/WaitEmpty，
		// 只有第二次确认空树才允许后续成功收口。
		forceCtx, forceCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		terminateErr := proc.Terminate(1)
		exitResult, waitErr := proc.Wait(forceCtx)
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			outcome.details["exitCode"] = exitResult.ExitCode
		}
		forceEmptyErr := proc.WaitEmpty(forceCtx)
		forceCancel()
		outcome.forced = forceEmptyErr == nil
		// 首次收口因上下文预算触发强杀时属于可恢复事实；真实非上下文
		// 错误仍须保留，避免强杀成功掩盖底层资源故障。
		resultErr = withoutExpectedCancellation(resultErr, false)
		if forceEmptyErr != nil {
			resultErr = errors.Join(resultErr, mapCleanupProcessError("wait_empty", waitEmptyErr))
		}
		resultErr = errors.Join(resultErr, mapCleanupProcessError("terminate", terminateErr))
		resultErr = errors.Join(resultErr, mapCleanupProcessError("wait", withoutExpectedCancellation(waitErr, forceCtx.Err() != nil)))
		resultErr = errors.Join(resultErr, mapCleanupProcessError("wait_empty", forceEmptyErr))
	} else {
		resultErr = errors.Join(resultErr, mapCleanupProcessError("wait_empty", waitEmptyErr))
	}
	resultErr = errors.Join(resultErr, mapCleanupProcessError("close", proc.Close()))
	if tx != nil {
		resourceCtx, resourceCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		if err := s.deps.State.RemoveBackendTransaction(resourceCtx, tx); err != nil {
			resultErr = errors.Join(resultErr, mapStateCleanupError(err))
		}
		resourceCancel()
	}
	if logger != nil {
		if err := logger.Close(); err != nil {
			resultErr = errors.Join(resultErr, mapLoggerCleanupError(err))
		}
	}
	outcome.err = withFailureDetailsExtra(resultErr, logger, proc, outcome.details)
	return outcome
}

func withoutExpectedCancellation(err error, keepDeadline bool) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		filtered := make([]error, 0, len(children))
		for _, child := range children {
			if kept := withoutExpectedCancellation(child, keepDeadline); kept != nil {
				filtered = append(filtered, kept)
			}
		}
		return errors.Join(filtered...)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !keepDeadline {
		return nil
	}
	return err
}

func mapCleanupProcessError(operation string, err error) error {
	if err == nil {
		return nil
	}
	details := map[string]any{"operation": operation}
	var typed *Error
	if errors.As(err, &typed) {
		for key, value := range typed.Details() {
			details[key] = value
		}
		return newCommittedError(typed.Code(), typed.Stage(), typed.Message(), details, err)
	}
	var coded interface {
		Code() protocol.Code
		Stage() protocol.Stage
		Message() string
		Details() map[string]any
	}
	if errors.As(err, &coded) {
		for key, value := range coded.Details() {
			details[key] = value
		}
		return newCommittedError(coded.Code(), coded.Stage(), coded.Message(), details, err)
	}
	var codeOnly interface{ Code() protocol.Code }
	if errors.As(err, &codeOnly) {
		return newCommittedError(codeOnly.Code(), protocol.StageBackendCleanup, "后端进程资源收口失败", details, err)
	}
	return newCommittedError(protocol.CodeBackendShutdownFailed, protocol.StageBackendCleanup, "后端进程资源收口失败", details, err)
}

func mapStateCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return newCommittedError(protocol.CodeStateWriteFailed, protocol.StageBackendCleanup, "后端状态收口失败", nil, err)
}

func mapMutexCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return markCommitted(mapDependencyError(protocol.StageBackendCleanup, protocol.CodeMutexOperationFailed, "后端 Mutex 收口失败", err))
}

func mapLoggerCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return newCommittedError(protocol.CodeInternalError, protocol.StageBackendCleanup, "后端运行日志收口失败", map[string]any{"sink": "runtime_log"}, err)
}

type processProbe struct {
	process     ManagedProcess
	uvPath      string
	pythonPaths []string
}

func (p processProbe) Exited() <-chan struct{} { return p.process.Exited() }

func (p processProbe) Healthy(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, errors.New("backend process probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	snapshot, err := p.process.Snapshot()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rootPID := p.process.PID()
	rootIndex := -1
	for index, info := range snapshot {
		if info.PID == rootPID {
			rootIndex = index
			if !sameExecutable(info.Executable, p.uvPath) {
				return false, nil
			}
			break
		}
	}
	if rootIndex < 0 {
		return false, nil
	}
	for _, info := range snapshot {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if info.PID == rootPID || !anyExecutable(info.Executable, p.pythonPaths) {
			continue
		}
		if descendantOf(info.PID, rootPID, snapshot) {
			return true, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func descendantOf(pid, root uint32, snapshot []process.Info) bool {
	parents := make(map[uint32]uint32, len(snapshot))
	for _, info := range snapshot {
		parents[info.PID] = info.ParentPID
	}
	seen := make(map[uint32]struct{}, len(snapshot))
	for pid != root {
		if pid == 0 {
			return false
		}
		if _, ok := seen[pid]; ok {
			return false
		}
		seen[pid] = struct{}{}
		parent, ok := parents[pid]
		if !ok {
			return false
		}
		pid = parent
	}
	return true
}

func sameExecutable(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func anyExecutable(path string, candidates []string) bool {
	for _, candidate := range candidates {
		if sameExecutable(path, candidate) {
			return true
		}
	}
	return false
}

var _ health.Probe = processProbe{}
