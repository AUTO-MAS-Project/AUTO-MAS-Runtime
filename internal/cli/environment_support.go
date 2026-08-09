package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

const m5TransactionCleanupTimeout = 30 * time.Second

func beginM5Transaction(
	ctx context.Context,
	store environmentStateStore,
	operationID string,
	command string,
	targetVersion string,
	stage protocol.Stage,
) (state.TransactionState, error) {
	transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID:   operationID,
		Command:       command,
		PID:           uint32(os.Getpid()),
		TargetVersion: targetVersion,
		Stage:         stage,
	})
	if err != nil {
		return state.TransactionState{}, stateStoreError(stage, err)
	}
	if err := store.WriteTransaction(ctx, state.TransactionMutation, transaction); err != nil {
		return state.TransactionState{}, stateStoreError(stage, err)
	}
	return transaction, nil
}

func advanceM5Transaction(
	ctx context.Context,
	store environmentStateStore,
	transaction *state.TransactionState,
	stage protocol.Stage,
) error {
	if transaction == nil {
		return stateStoreError(stage, errors.New("m5 transaction is unavailable"))
	}
	transaction.Stage = stage
	if err := store.WriteTransaction(ctx, state.TransactionMutation, *transaction); err != nil {
		return stateStoreError(stage, err)
	}
	return nil
}

func removeM5Transaction(ctx context.Context, store environmentStateStore) error {
	snapshot, err := store.ReadTransaction(ctx, state.TransactionMutation)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return stateStoreError(protocol.StageWorkspaceCleanup, err)
	}
	if err := store.RemoveTransaction(ctx, snapshot); err != nil {
		return stateStoreError(protocol.StageWorkspaceCleanup, err)
	}
	return nil
}

// recoverM5Transaction 在取得 mutation 租约后处理上次进程留下的单槽事务。
// 只有 PID 已退出且 Mutex 已确认空闲时才允许恢复；危险阶段必须先把稳定状态
// 失效为 environment_broken，再条件删除事务。M4 workspace 现场保留给其专用恢复器。
func recoverM5Transaction(ctx context.Context, store environmentStateStore, layout *config.Layout) error {
	if store == nil || layout == nil {
		return stateStoreError(protocol.StageWorkspaceCleanup, errors.New("m5 state store is unavailable"))
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionMutation)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return stateStoreError(protocol.StageWorkspaceCleanup, err)
	}
	transaction := snapshot.State()
	inspection := state.InspectTransaction(
		ctx,
		state.TransactionMutation,
		transaction,
		state.NewSystemPIDProbe(),
		m5FreeMutexProbe{},
	)
	if inspection.ProbeError != nil {
		return stateStoreError(transaction.Stage, inspection.ProbeError)
	}
	switch inspection.Activity {
	case state.ActivityStale:
		cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
		defer cancelCleanup()
		owned, invalidatesEnvironment := classifyStaleM5Stage(transaction.Stage)
		if !owned {
			return staleM5TransactionOwnershipError(transaction)
		}
		if invalidatesEnvironment {
			if err := persistStaleM5BrokenState(cleanupContext, store, layout, transaction); err != nil {
				return err
			}
		}
		if err := store.RemoveTransaction(cleanupContext, snapshot); err != nil {
			return stateStoreError(protocol.StageWorkspaceCleanup, err)
		}
		return nil
	case state.ActivityActive, state.ActivityInconsistent, state.ActivityUnknown:
		return &commandError{
			code:    protocol.CodeMutationInProgress,
			stage:   transaction.Stage,
			message: "已有运行中的环境变更操作",
			details: map[string]any{"command": transaction.Command, "pid": transaction.PID},
			cause:   errors.New("m5 mutation transaction is still active"),
		}
	default:
		return stateStoreError(protocol.StageWorkspaceCleanup, errors.New("m5 transaction activity is invalid"))
	}
}

func classifyStaleM5Stage(stage protocol.Stage) (owned bool, invalidatesEnvironment bool) {
	switch stage {
	case protocol.StageBootstrap, protocol.StageRepair,
		protocol.StageUVCheck, protocol.StagePythonCheck,
		protocol.StageDependenciesCheck:
		return true, false
	case protocol.StageUVDownload, protocol.StageUVVerify,
		protocol.StagePythonInstall, protocol.StageDependenciesSync,
		protocol.StageDependenciesRebuild:
		return true, true
	default:
		return false, false
	}
}

func persistStaleM5BrokenState(
	ctx context.Context,
	store environmentStateStore,
	layout *config.Layout,
	transaction state.TransactionState,
) error {
	current, err := store.ReadEnvironment(ctx)
	if errors.Is(err, state.ErrNotFound) {
		// 首次安装还没有稳定 ready 声明，无需构造不存在的 active revision。
		return nil
	}
	if err != nil {
		return stateStoreError(transaction.Stage, err)
	}
	revision, ok := activeEnvironmentRevision(current)
	if !ok {
		return stateStoreError(transaction.Stage, errors.New("stale m5 transaction has no active revision"))
	}
	if staleM5ToolsRequired(transaction.Stage) &&
		transaction.TargetVersion != "" && transaction.TargetVersion != revision.Version() {
		return staleM5TransactionOwnershipError(transaction)
	}

	stage := transaction.Stage
	python := uv.PythonSpec{}
	uvExecutable := ""
	if staleM5ToolsRequired(stage) {
		uvExecutable, err = layout.UVExecutable(uv.FixedVersion)
		if err != nil {
			return stateStoreError(stage, err)
		}
		python, err = uv.ReadPythonSpec(ctx, layout, layout.RepoDir())
		if err != nil {
			// 进入工具修改阶段本应已经解析过项目契约；若现场文件已无法复核，
			// 先以 python.check + unknown 工具事实失效旧 ready，绝不保留假就绪。
			stage = protocol.StagePythonCheck
			python = uv.PythonSpec{}
		}
	}
	persisted, err := persistBrokenState(
		ctx,
		store,
		layout,
		current,
		revision,
		uvExecutable,
		python,
		nil,
		stage,
		map[string]any{"exitCode": 1},
	)
	if err != nil {
		return err
	}
	if !persisted {
		return stateStoreError(stage, errors.New("stale m5 transaction could not invalidate environment"))
	}
	return nil
}

func staleM5ToolsRequired(stage protocol.Stage) bool {
	switch stage {
	case protocol.StagePythonInstall, protocol.StageDependenciesSync,
		protocol.StageDependenciesRebuild:
		return true
	default:
		return false
	}
}

func staleM5TransactionOwnershipError(transaction state.TransactionState) error {
	return &commandError{
		code:    protocol.CodeUpdateStateAmbiguous,
		stage:   transaction.Stage,
		message: "遗留变更事务需要专用恢复",
		details: map[string]any{"command": transaction.Command},
		cause:   errors.New("stale mutation transaction is outside m5 recovery ownership"),
	}
}

type m5FreeMutexProbe struct{}

func (m5FreeMutexProbe) Probe(ctx context.Context, kind state.MutexKind) (state.MutexProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return state.MutexProbeResult{}, err
	}
	if !kind.Valid() {
		return state.MutexProbeResult{}, errors.New("invalid mutation mutex kind")
	}
	return state.MutexProbeResult{}, nil
}

var _ state.MutexProbe = m5FreeMutexProbe{}

func retainM5TransactionOnFailure(err error, stage protocol.Stage) bool {
	if findOperationErrorCode(err, protocol.CodeStateWriteFailed) != nil {
		return true
	}
	_, invalidatesEnvironment := classifyStaleM5Stage(stage)
	return err != nil && invalidatesEnvironment
}

func m5TransactionCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), m5TransactionCleanupTimeout)
}

func uvLogLine(logger workspaceLogger) uv.LineFunc {
	if logger == nil {
		return nil
	}
	return func(ctx context.Context, stream, line string) error {
		_, err := logger.Record(ctx, logging.LevelInfo, "uv 输出", map[string]any{
			"stream": stream,
			"line":   line,
		})
		return err
	}
}

func ensureM5UV(
	ctx context.Context,
	service environmentService,
	operationID string,
	policy mirror.Policy,
	line uv.LineFunc,
) (string, error) {
	if withLine, ok := service.(interface {
		EnsureUVWithLine(context.Context, string, mirror.Policy, uv.LineFunc) (string, error)
	}); ok {
		return withLine.EnsureUVWithLine(ctx, operationID, policy, line)
	}
	return service.EnsureUV(ctx, operationID, policy)
}

func repairM5UV(
	ctx context.Context,
	service environmentService,
	operationID string,
	policy mirror.Policy,
	line uv.LineFunc,
) (string, error) {
	if withLine, ok := service.(interface {
		RepairUVWithLine(context.Context, string, mirror.Policy, uv.LineFunc) (string, error)
	}); ok {
		return withLine.RepairUVWithLine(ctx, operationID, policy, line)
	}
	return service.RepairUV(ctx, operationID, policy)
}

func checkM5UV(
	ctx context.Context,
	service environmentService,
	line uv.LineFunc,
) (bool, error) {
	if withLine, ok := service.(interface {
		CheckUVWithLine(context.Context, uv.LineFunc) (bool, error)
	}); ok {
		return withLine.CheckUVWithLine(ctx, line)
	}
	return service.CheckUV(ctx)
}

func openM5Logger(ctx context.Context, deps *deps, command, operationID string) (workspaceLogger, error) {
	logger, err := deps.options.workspaceLoggerFactory(
		ctx,
		deps.global.layout,
		deps.io.Err,
		command,
		operationID,
		deps.options.clock,
	)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("m5 logger is unavailable")
	}
	return logger, nil
}

func acquireMutation(ctx context.Context, deps *deps) (gitrepo.MutationLease, gitrepo.MutationCoordinator, error) {
	coordinator, err := deps.options.mutationCoordinatorFactory(ctx, deps.global.layout)
	if err != nil {
		return nil, nil, mutationAcquireError(err)
	}
	if coordinator == nil {
		return nil, nil, mutationAcquireError(errors.New("mutation coordinator is nil"))
	}
	lease, err := coordinator.AcquireMutation(ctx)
	if err != nil || lease == nil {
		closeErr := coordinator.Close()
		if err == nil {
			err = errors.New("mutation coordinator returned an empty lease")
		}
		return nil, nil, mutationAcquireError(errors.Join(err, closeErr))
	}
	return lease, coordinator, nil
}

func closeMutation(lease gitrepo.MutationLease, coordinator gitrepo.MutationCoordinator) error {
	var closeErr error
	if lease != nil {
		closeErr = errors.Join(closeErr, lease.Close())
	}
	if coordinator != nil {
		closeErr = errors.Join(closeErr, coordinator.Close())
	}
	return closeErr
}

func readEnvironmentOrUninitialized(
	ctx context.Context,
	store environmentStateStore,
) (state.EnvironmentState, error) {
	value, err := store.ReadEnvironment(ctx)
	if errors.Is(err, state.ErrNotFound) {
		return state.EnvironmentState{
			SchemaVersion: state.SchemaVersion,
			Status:        protocol.StateUninitialized,
		}, nil
	}
	if err != nil {
		return state.EnvironmentState{}, stateStoreError(protocol.StageBootstrap, err)
	}
	return value, nil
}

func transitionM5State(
	emitter *protocol.Emitter,
	machine *protocol.LifecycleMachine,
	stage protocol.Stage,
	next protocol.StateStatus,
	message string,
) error {
	if machine == nil {
		return &commandError{
			code:    protocol.CodeInternalError,
			stage:   stage,
			message: "生命周期状态机不可用",
			details: map[string]any{},
			cause:   errors.New("m5 lifecycle machine is unavailable"),
		}
	}
	if err := machine.Transition(next); err != nil {
		return &commandError{
			code:    protocol.CodeInternalError,
			stage:   stage,
			message: "生命周期状态迁移失败",
			details: map[string]any{"from": machine.Current(), "to": next},
			cause:   err,
		}
	}
	return emitM5State(emitter, stage, next, message)
}

func rollbackM5Preparation(
	emitter *protocol.Emitter,
	machine *protocol.LifecycleMachine,
	stage protocol.Stage,
	message string,
) error {
	if machine == nil {
		return errors.New("m5 lifecycle machine is unavailable")
	}
	if err := machine.RollbackPreparation(); err != nil {
		return &commandError{
			code:    protocol.CodeInternalError,
			stage:   stage,
			message: "生命周期状态回滚失败",
			details: map[string]any{},
			cause:   err,
		}
	}
	return emitM5State(emitter, stage, machine.Current(), message)
}

func persistM5FailureWithLifecycle(
	ctx context.Context,
	emitter *protocol.Emitter,
	store environmentStateStore,
	layout *config.Layout,
	initial state.EnvironmentState,
	revision gitrepo.Revision,
	uvExecutable string,
	python uv.PythonSpec,
	logger workspaceLogger,
	machine *protocol.LifecycleMachine,
	fallbackStage protocol.Stage,
	cause error,
) error {
	persistContext, cancel := m5TransactionCleanupContext(ctx)
	defer cancel()
	stage, persisted, persistErr := persistBootstrapFailureState(
		persistContext,
		store,
		layout,
		initial,
		revision,
		uvExecutable,
		python,
		logger,
		fallbackStage,
		cause,
	)
	if persistErr != nil {
		return errors.Join(persistErr, cause)
	}
	if persisted {
		var transitionErr error
		if err := machine.Transition(protocol.StateEnvironmentBroken); err != nil {
			transitionErr = &commandError{
				code:    protocol.CodeInternalError,
				stage:   stage,
				message: "生命周期状态迁移失败",
				details: map[string]any{},
				cause:   err,
			}
		}
		var emitErr error
		if transitionErr == nil {
			emitErr = emitM5State(emitter, stage, protocol.StateEnvironmentBroken, "运行环境已损坏")
		}
		removeErr := removeM5Transaction(persistContext, store)
		return errors.Join(removeErr, transitionErr, emitErr, cause)
	}
	return errors.Join(rollbackM5Preparation(emitter, machine, stage, "运行环境保持原稳定状态"), cause)
}

func activeEnvironmentRevision(value state.EnvironmentState) (gitrepo.Revision, bool) {
	version := value.LastSuccessful.Version
	commit := value.LastSuccessful.Commit
	if value.Status == protocol.StateEnvironmentBroken && value.Broken != nil {
		version = value.Broken.TargetVersion
		commit = value.Broken.Commit
	}
	if version == "" || commit == "" {
		return gitrepo.Revision{}, false
	}
	revision, err := gitrepo.NewRevision(version, "release/"+version, commit, "state")
	if err != nil {
		return gitrepo.Revision{}, false
	}
	return revision, true
}

func persistBootstrapFailureState(
	ctx context.Context,
	store environmentStateStore,
	layout *config.Layout,
	initial state.EnvironmentState,
	revision gitrepo.Revision,
	uvExecutable string,
	python uv.PythonSpec,
	logger workspaceLogger,
	fallbackStage protocol.Stage,
	cause error,
) (protocol.Stage, bool, error) {
	_, stage, _, details := classifyFailure(cause, fallbackStage)
	persistCtx, cancel := m5TransactionCleanupContext(ctx)
	defer cancel()
	persisted, persistErr := persistBrokenState(
		persistCtx,
		store,
		layout,
		initial,
		revision,
		uvExecutable,
		python,
		logger,
		stage,
		details,
	)
	return stage, persisted, persistErr
}

func persistBrokenState(
	ctx context.Context,
	store environmentStateStore,
	layout *config.Layout,
	initial state.EnvironmentState,
	revision gitrepo.Revision,
	uvExecutable string,
	python uv.PythonSpec,
	logger workspaceLogger,
	stage protocol.Stage,
	details map[string]any,
) (bool, error) {
	if layout == nil || revision.Version() == "" || revision.Commit() == "" || !persistableEnvironmentFailureStage(stage) {
		return false, nil
	}
	lastSuccessful := initial.LastSuccessful
	if current, readErr := store.ReadEnvironment(ctx); readErr == nil {
		lastSuccessful = current.LastSuccessful
	} else if !errors.Is(readErr, state.ErrNotFound) {
		return false, stateStoreError(stage, readErr)
	}
	logPath := ""
	if logger != nil {
		logPath = logger.LogPath()
	}
	if logPath == "" {
		logPath = filepath.Join(layout.RuntimeLogDir(), "m5.log")
	}
	exit := detailsExitCode(details)
	if exit <= 0 {
		exit = 1
	}
	pythonVersion := ""
	if python.Version != (uv.PythonVersion{}) {
		pythonVersion = python.Version.String()
	}
	uvVersion := uv.FixedVersion
	if uvExecutable == "" && (stage == protocol.StageUVCheck ||
		stage == protocol.StageUVDownload || stage == protocol.StageUVVerify) {
		uvVersion = ""
	}
	broken := state.BrokenEnvironment{
		TargetVersion: revision.Version(),
		Branch:        revision.Branch(),
		Commit:        revision.Commit(),
		PythonVersion: pythonVersion,
		UVVersion:     uvVersion,
		Reason:        state.ReasonOperationFailed,
		Stage:         stage,
		ExitCode:      exit,
		LogPath:       logPath,
	}
	value, err := store.NewBrokenEnvironment(lastSuccessful, broken)
	if err != nil {
		return false, stateStoreError(stage, err)
	}
	if err := store.WriteEnvironment(ctx, value); err != nil {
		return false, stateStoreError(stage, err)
	}
	return true, nil
}

func persistableEnvironmentFailureStage(stage protocol.Stage) bool {
	switch stage {
	case protocol.StageUVCheck, protocol.StageUVDownload, protocol.StageUVVerify,
		protocol.StagePythonCheck, protocol.StagePythonInstall,
		protocol.StageDependenciesCheck, protocol.StageDependenciesSync,
		protocol.StageDependenciesRebuild:
		return true
	default:
		return false
	}
}

func detailsExitCode(details map[string]any) int {
	value, ok := details["exitCode"]
	if !ok {
		return 0
	}
	switch value := value.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := strconv.Atoi(value.String())
		return parsed
	default:
		return 0
	}
}

func stateStoreError(stage protocol.Stage, cause error) error {
	return &commandError{
		code:    protocol.CodeStateWriteFailed,
		stage:   stage,
		message: "环境状态写入失败",
		details: map[string]any{},
		cause:   cause,
	}
}

func mutationAcquireError(cause error) error {
	return &commandError{
		code:    protocol.CodeMutexOperationFailed,
		stage:   protocol.StageBootstrap,
		message: "无法获取运行环境变更锁",
		details: map[string]any{},
		cause:   cause,
	}
}

func mutationCloseError(cause error) error {
	return &commandError{
		code:    protocol.CodeMutexOperationFailed,
		stage:   protocol.StageWorkspaceCleanup,
		message: "运行环境变更锁收口失败",
		details: map[string]any{},
		cause:   cause,
	}
}

func emitM5Progress(
	emitter *protocol.Emitter,
	stage protocol.Stage,
	status protocol.ProgressStatus,
	message string,
) error {
	if err := emitter.EmitProgress(protocol.ProgressEvent{Stage: stage, Status: status, Message: message}); err != nil {
		return &commandError{
			code:    protocol.CodeOutputWriteFailed,
			stage:   stage,
			message: "协议输出失败",
			details: map[string]any{},
			cause:   err,
		}
	}
	return nil
}

func emitM5State(
	emitter *protocol.Emitter,
	stage protocol.Stage,
	status protocol.StateStatus,
	message string,
) error {
	if err := emitter.EmitState(protocol.StateEvent{Stage: stage, Status: status, Message: message}); err != nil {
		return &commandError{
			code:    protocol.CodeOutputWriteFailed,
			stage:   stage,
			message: "协议输出失败",
			details: map[string]any{},
			cause:   err,
		}
	}
	return nil
}

var _ filesystem.Auditor = (*workspaceLogBinding)(nil)
var _ environmentService = (*uv.EnvironmentService)(nil)
var _ environmentService = (*uv.ProductionEnvironment)(nil)
