package cli

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// bootstrapCommand 注册固定顺序的 M5 准备编排。
func bootstrapCommand(deps *deps) *cobra.Command {
	command := &cobra.Command{
		Use:   "bootstrap",
		Short: "准备受管后端环境",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageBootstrap,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (success sessionSuccess, returnErr error) {
					return runBootstrap(ctx, deps, cmd, emitter)
				},
			)
			return nil
		},
	}
	command.Flags().String("version", "", "目标版本（例如 v5.4.0-beta.1）")
	return command
}

func runBootstrap(
	ctx context.Context,
	deps *deps,
	command *cobra.Command,
	emitter *protocol.Emitter,
) (success sessionSuccess, returnErr error) {
	version, err := command.Flags().GetString("version")
	if err != nil || version == "" {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeInvalidVersion,
			stage:   protocol.StageBootstrap,
			message: "目标版本无效",
			details: map[string]any{},
			cause:   errors.New("bootstrap requires exactly one version"),
		}
	}
	target, err := gitrepo.ParseTarget(version)
	if err != nil {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeInvalidVersion,
			stage:   protocol.StageBootstrap,
			message: "目标版本无效",
			details: map[string]any{},
			cause:   err,
		}
	}
	store, err := deps.options.environmentStateStoreFactory(ctx, deps.global.layout, deps.options.clock)
	if err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageBootstrap, err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, stateStoreError(protocol.StageWorkspaceCleanup, closeErr))
		}
	}()
	lease, coordinator, err := acquireMutation(ctx, deps)
	if err != nil {
		return sessionSuccess{}, err
	}
	defer func() {
		if closeErr := closeMutation(lease, coordinator); closeErr != nil {
			returnErr = errors.Join(returnErr, mutationCloseError(closeErr))
		}
	}()
	if err := recoverM5Transaction(ctx, store, deps.global.layout); err != nil {
		return sessionSuccess{}, err
	}
	initial, err := readEnvironmentOrUninitialized(ctx, store)
	if err != nil {
		return sessionSuccess{}, err
	}
	machine, err := protocol.NewLifecycleMachine(initial.Status)
	if err != nil {
		return sessionSuccess{}, err
	}
	service, err := deps.options.environmentFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	operationLogger, err := openM5Logger(ctx, deps, "bootstrap", emitter.OperationID())
	if err != nil {
		return sessionSuccess{}, err
	}
	defer func() {
		if closeErr := operationLogger.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, stateStoreError(protocol.StageWorkspaceCleanup, closeErr))
		}
	}()
	uvTransaction, err := beginM5Transaction(
		ctx,
		store,
		emitter.OperationID(),
		"bootstrap",
		version,
		protocol.StageUVCheck,
	)
	if err != nil {
		return sessionSuccess{}, err
	}
	uvTransactionActive := true
	defer func() {
		if !uvTransactionActive || retainM5TransactionOnFailure(returnErr, uvTransaction.Stage) {
			return
		}
		cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
		defer cancelCleanup()
		if cleanupErr := removeM5Transaction(cleanupContext, store); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()

	if err := transitionM5State(emitter, machine, protocol.StageUVCheck, protocol.StatePreparingUV, "正在准备固定版本 uv"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &uvTransaction, protocol.StageUVDownload); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageUVDownload, protocol.ProgressRunning, "正在准备固定版本 uv"); err != nil {
		return sessionSuccess{}, err
	}
	uvExecutable, err := ensureM5UV(
		ctx,
		service,
		emitter.OperationID(),
		deps.global.mirrorPolicy,
		uvLogLine(operationLogger),
	)
	if err != nil {
		revision, _ := activeEnvironmentRevision(initial)
		failure := persistM5FailureWithLifecycle(
			ctx,
			emitter,
			store,
			deps.global.layout,
			initial,
			revision,
			"",
			uv.PythonSpec{},
			operationLogger,
			machine,
			protocol.StageUVDownload,
			err,
		)
		return sessionSuccess{}, failure
	}
	if err := advanceM5Transaction(ctx, store, &uvTransaction, protocol.StageUVVerify); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageUVVerify, protocol.ProgressSucceeded, "固定版本 uv 已校验"); err != nil {
		return sessionSuccess{}, err
	}
	transactionCleanupContext, cancelTransactionCleanup := m5TransactionCleanupContext(ctx)
	removeErr := removeM5Transaction(transactionCleanupContext, store)
	cancelTransactionCleanup()
	if removeErr != nil {
		return sessionSuccess{}, removeErr
	}
	uvTransactionActive = false
	if err := transitionM5State(emitter, machine, protocol.StageWorkspaceCheck, protocol.StateSyncingRepository, "正在同步后端仓库"); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageWorkspaceClone, protocol.ProgressRunning, "正在同步后端仓库"); err != nil {
		return sessionSuccess{}, err
	}

	workspace, err := deps.options.workspaceFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	control := workspaceControlFromContext(ctx)
	binding := &workspaceLogBinding{}
	workspaceResult, err := workspace.Sync(ctx, gitrepo.SyncRequest{
		Target:        target,
		Policy:        deps.global.mirrorPolicy,
		OperationID:   emitter.OperationID(),
		PID:           uint32(os.Getpid()),
		MutationLease: lease,
		Emitter:       directWorkspaceEmitter{emitter: emitter, suppressWorkspaceStates: true},
		LoggerFactory: func(loggerContext context.Context, commandName, operationID string) (gitrepo.OperationLogger, error) {
			logger, loggerErr := deps.options.workspaceLoggerFactory(
				loggerContext,
				deps.global.layout,
				deps.io.Err,
				commandName,
				operationID,
				deps.options.clock,
			)
			if loggerErr == nil {
				binding.Set(logger)
			}
			return logger, loggerErr
		},
		Auditor: binding,
		Clock:   deps.options.clock,
		StageReporter: func(stage protocol.Stage) {
			if control != nil {
				control.SetStage(stage)
			}
		},
		ControlCommandID: func() string {
			if control == nil {
				return ""
			}
			return control.CommandID()
		},
	})
	if err != nil {
		if committed := findCommittedOperationError(err); committed != nil {
			return sessionSuccess{}, errors.Join(
				err,
				transitionM5State(emitter, machine, protocol.StageWorkspaceSwap, protocol.StateEnvironmentBroken, "仓库已替换，运行环境已失效"),
			)
		}
		return sessionSuccess{}, errors.Join(
			err,
			rollbackM5Preparation(emitter, machine, protocol.StageWorkspaceCleanup, "仓库同步未改变当前工作区"),
		)
	}
	if err := emitM5Progress(emitter, protocol.StageWorkspaceClone, protocol.ProgressSucceeded, "后端仓库已同步"); err != nil {
		return sessionSuccess{}, err
	}
	revision := workspaceResult.Revision
	if revision.Version() == "" || revision.Commit() == "" {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeGitRepositoryInvalid,
			stage:   protocol.StageWorkspaceVerify,
			message: "同步后未取得有效仓库版本",
			details: map[string]any{},
			cause:   errors.New("workspace sync returned an empty revision"),
		}
	}
	if err := transitionM5State(emitter, machine, protocol.StagePythonCheck, protocol.StatePreparingPython, "正在准备受管 Python"); err != nil {
		return sessionSuccess{}, err
	}
	transaction, err := beginM5Transaction(
		ctx,
		store,
		emitter.OperationID(),
		"bootstrap",
		revision.Version(),
		protocol.StagePythonCheck,
	)
	if err != nil {
		return sessionSuccess{}, err
	}
	transactionActive := true
	defer func() {
		if !transactionActive || retainM5TransactionOnFailure(returnErr, transaction.Stage) {
			return
		}
		cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
		defer cancelCleanup()
		if cleanupErr := removeM5Transaction(cleanupContext, store); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := emitM5Progress(emitter, protocol.StagePythonCheck, protocol.ProgressRunning, "正在读取项目 Python 契约"); err != nil {
		return sessionSuccess{}, err
	}
	pythonSpec, err := service.ReadPythonSpec(ctx, deps.global.layout.RepoDir())
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, uvExecutable, uv.PythonSpec{}, operationLogger, machine, protocol.StagePythonCheck, err)
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StagePythonInstall); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StagePythonInstall, protocol.ProgressRunning, "正在准备受管 Python"); err != nil {
		return sessionSuccess{}, err
	}
	pythonResult, err := service.PreparePython(ctx, uv.PythonRequest{
		ProjectDir:       deps.global.layout.RepoDir(),
		PythonInstallDir: deps.global.layout.PythonDir(),
		ProjectEnvDir:    deps.global.layout.VenvDir(),
		CacheDir:         deps.global.layout.UVCacheDir(),
		Branch:           revision.Branch(),
		Commit:           revision.Commit(),
		MirrorPolicy:     deps.global.mirrorPolicy,
		Line:             uvLogLine(operationLogger),
	})
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, uvExecutable, pythonSpec, operationLogger, machine, protocol.StagePythonInstall, err)
	}
	if err := emitM5Progress(emitter, protocol.StagePythonInstall, protocol.ProgressSucceeded, "受管 Python 已就绪"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageDependenciesCheck); err != nil {
		return sessionSuccess{}, err
	}
	if err := transitionM5State(emitter, machine, protocol.StageDependenciesSync, protocol.StateSyncingEnvironment, "正在同步锁定依赖"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageDependenciesSync); err != nil {
		return sessionSuccess{}, err
	}
	dependencyResult, err := service.SyncDependencies(ctx, uv.DependenciesRequest{
		ProjectDir:    deps.global.layout.RepoDir(),
		ProjectEnvDir: deps.global.layout.VenvDir(),
		PythonVersion: pythonResult.Spec.Version.String(),
		OperationID:   emitter.OperationID(),
		Branch:        revision.Branch(),
		Commit:        revision.Commit(),
		MirrorPolicy:  deps.global.mirrorPolicy,
		Line:          uvLogLine(operationLogger),
	})
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, uvExecutable, pythonResult.Spec, operationLogger, machine, protocol.StageDependenciesSync, err)
	}
	ready, err := store.NewReadyEnvironment(revision.Version(), revision.Commit())
	if err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageDependenciesSync, err)
	}
	if err := store.WriteEnvironment(ctx, ready); err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageDependenciesSync, err)
	}
	transactionCleanupContext, cancelTransactionCleanup = m5TransactionCleanupContext(ctx)
	removeErr = removeM5Transaction(transactionCleanupContext, store)
	cancelTransactionCleanup()
	if removeErr != nil {
		return sessionSuccess{}, removeErr
	}
	transactionActive = false
	if err := transitionM5State(emitter, machine, protocol.StageDependenciesSync, protocol.StateReadyToStart, "运行环境已就绪"); err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "运行环境准备完成",
		status:  string(protocol.StateReadyToStart),
		details: map[string]any{
			"version":         revision.Version(),
			"branch":          revision.Branch(),
			"commit":          revision.Commit(),
			"uvExecutable":    uvExecutable,
			"uvVersion":       uv.FixedVersion,
			"pythonVersion":   pythonResult.Spec.Version.String(),
			"lockfileChecked": dependencyResult.LockfileChecked,
			"synchronized":    dependencyResult.Synchronized,
		},
	}, nil
}

type directWorkspaceEmitter struct {
	emitter                 *protocol.Emitter
	suppressWorkspaceStates bool
}

func (e directWorkspaceEmitter) EmitProgress(event protocol.ProgressEvent) error {
	return e.emitter.EmitProgress(event)
}

func (e directWorkspaceEmitter) EmitState(event protocol.StateEvent) error {
	if e.suppressWorkspaceStates {
		// workspace sync 的稳定状态由 M4 持久化，但 bootstrap 必须拥有整条
		// M5 生命周期的唯一事件序列，避免 swap 后的 environment_broken 或
		// no-op 的旧稳定态打断 preparing_python 等后续迁移。
		return nil
	}
	return e.emitter.EmitState(event)
}

var _ gitrepo.WorkspaceEmitter = directWorkspaceEmitter{}
