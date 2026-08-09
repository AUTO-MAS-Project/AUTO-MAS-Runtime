package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// dependenciesGroup 注册主项目依赖的检查、同步和重建命令。
func dependenciesGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "dependencies", Short: "主项目依赖操作"}
	group.AddCommand(
		dependenciesCheckCommand(deps),
		dependenciesSyncCommand(deps),
		dependenciesRebuildCommand(deps),
	)
	return group
}

func dependenciesCheckCommand(deps *deps) *cobra.Command {
	return newDependencyCommand(deps, "check", "只读检查依赖环境", protocol.StageDependenciesCheck, false, false)
}

func dependenciesSyncCommand(deps *deps) *cobra.Command {
	return newDependencyCommand(deps, "sync", "按锁文件同步主项目依赖", protocol.StageDependenciesSync, true, false)
}

func dependenciesRebuildCommand(deps *deps) *cobra.Command {
	return newDependencyCommand(deps, "rebuild", "重建主项目环境", protocol.StageDependenciesRebuild, true, true)
}

func newDependencyCommand(
	deps *deps,
	use, short string,
	stage protocol.Stage,
	mutating bool,
	rebuild bool,
) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				stage,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					return runDependencyAction(ctx, deps, emitter, mutating, rebuild)
				},
			)
			return nil
		},
	}
}

func runDependencyAction(
	ctx context.Context,
	deps *deps,
	emitter *protocol.Emitter,
	mutating bool,
	rebuild bool,
) (success sessionSuccess, returnErr error) {
	if mutating {
		return runMutatingDependencyAction(ctx, deps, emitter, rebuild)
	}
	workspace, err := deps.options.workspaceFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	check, err := workspace.Check(ctx)
	if err != nil {
		return sessionSuccess{}, err
	}
	if err := requireHealthyWorkspace(check); err != nil {
		return sessionSuccess{}, err
	}
	service, err := deps.options.environmentFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	logger, err := openM5Logger(ctx, deps, "dependencies check", emitter.OperationID())
	if err != nil {
		return sessionSuccess{}, err
	}
	defer func() {
		if closeErr := logger.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, stateStoreError(protocol.StageWorkspaceCleanup, closeErr))
		}
	}()
	if err := requireUVReady(ctx, service, uvLogLine(logger)); err != nil {
		return sessionSuccess{}, err
	}
	pythonRequest := dependencyPythonRequest(deps, check)
	pythonRequest.Line = uvLogLine(logger)
	python, err := service.CheckPython(ctx, pythonRequest)
	if err != nil {
		return sessionSuccess{}, err
	}
	request := dependencyRequest(deps, emitter, check, python.Spec.Version.String(), uvLogLine(logger))
	result, err := service.CheckDependencies(ctx, request)
	if err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "依赖检查完成",
		details: map[string]any{"version": check.Version, "commit": check.Commit, "lockfileChecked": result.LockfileChecked},
	}, nil
}

func runMutatingDependencyAction(
	ctx context.Context,
	deps *deps,
	emitter *protocol.Emitter,
	rebuild bool,
) (success sessionSuccess, returnErr error) {
	stage := stageForDependencyAction(rebuild)
	store, err := deps.options.environmentStateStoreFactory(ctx, deps.global.layout, deps.options.clock)
	if err != nil {
		return sessionSuccess{}, stateStoreError(stage, err)
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

	workspace, err := deps.options.workspaceFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	check, err := workspace.Check(ctx)
	if err != nil {
		return sessionSuccess{}, err
	}
	if err := requireHealthyWorkspace(check); err != nil {
		return sessionSuccess{}, err
	}
	revision, err := gitrepo.NewRevision(check.Version, check.Branch, check.Commit, check.Source)
	if err != nil {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeGitRepositoryInvalid,
			stage:   protocol.StageWorkspaceCheck,
			message: "受管仓库 revision 无效",
			details: map[string]any{},
			cause:   err,
		}
	}
	transaction, err := beginM5Transaction(
		ctx,
		store,
		emitter.OperationID(),
		dependencyTransactionCommand(rebuild),
		check.Version,
		protocol.StageDependenciesCheck,
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
	logger, err := openM5Logger(ctx, deps, dependencyTransactionCommand(rebuild), emitter.OperationID())
	if err != nil {
		return sessionSuccess{}, err
	}
	defer func() {
		if closeErr := logger.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, stateStoreError(protocol.StageWorkspaceCleanup, closeErr))
		}
	}()
	service, err := deps.options.environmentFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	if err := transitionM5State(emitter, machine, stage, protocol.StateSyncingEnvironment, "正在检查并同步锁定依赖"); err != nil {
		return sessionSuccess{}, err
	}
	if err := requireUVReady(ctx, service, uvLogLine(logger)); err != nil {
		failure := persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, "", uv.PythonSpec{}, logger, machine, protocol.StageUVCheck, err)
		return sessionSuccess{}, failure
	}
	pythonSpec, err := service.ReadPythonSpec(ctx, deps.global.layout.RepoDir())
	if err != nil {
		failure := persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, "", uv.PythonSpec{}, logger, machine, protocol.StagePythonCheck, err)
		return sessionSuccess{}, failure
	}
	pythonRequest := dependencyPythonRequest(deps, check)
	pythonRequest.Line = uvLogLine(logger)
	python, err := service.CheckPython(ctx, pythonRequest)
	if err != nil {
		failure := persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, "", pythonSpec, logger, machine, protocol.StagePythonCheck, err)
		return sessionSuccess{}, failure
	}
	request := dependencyRequest(deps, emitter, check, python.Spec.Version.String(), uvLogLine(logger))
	if rebuild {
		if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageDependenciesRebuild); err != nil {
			return sessionSuccess{}, err
		}
		if err := emitM5Progress(emitter, protocol.StageDependenciesRebuild, protocol.ProgressRunning, "正在删除并重建受管 venv"); err != nil {
			return sessionSuccess{}, err
		}
		if _, err := service.RebuildDependencies(ctx, request); err != nil {
			failure := persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, "", python.Spec, logger, machine, protocol.StageDependenciesRebuild, err)
			return sessionSuccess{}, failure
		}
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageDependenciesSync); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageDependenciesSync, protocol.ProgressRunning, "正在同步锁定依赖"); err != nil {
		return sessionSuccess{}, err
	}
	result, err := service.SyncDependencies(ctx, request)
	if err != nil {
		failure := persistM5FailureWithLifecycle(ctx, emitter, store, deps.global.layout, initial, revision, "", python.Spec, logger, machine, protocol.StageDependenciesSync, err)
		return sessionSuccess{}, failure
	}
	if err := emitM5Progress(emitter, protocol.StageDependenciesSync, protocol.ProgressSucceeded, "锁定依赖已同步"); err != nil {
		return sessionSuccess{}, err
	}
	ready, err := store.NewReadyEnvironment(check.Version, check.Commit)
	if err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageDependenciesSync, err)
	}
	if err := store.WriteEnvironment(ctx, ready); err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageDependenciesSync, err)
	}
	cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
	removeErr := removeM5Transaction(cleanupContext, store)
	cancelCleanup()
	if removeErr != nil {
		return sessionSuccess{}, removeErr
	}
	transactionActive = false
	if err := transitionM5State(emitter, machine, protocol.StageDependenciesSync, protocol.StateReadyToStart, "运行环境已就绪"); err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "主项目依赖同步完成",
		status:  string(protocol.StateReadyToStart),
		details: map[string]any{"version": check.Version, "commit": check.Commit, "synchronized": result.Synchronized},
	}, nil
}

func dependencyTransactionCommand(rebuild bool) string {
	if rebuild {
		return "dependencies rebuild"
	}
	return "dependencies sync"
}

func requireHealthyWorkspace(check gitrepo.CheckResult) error {
	if check.Healthy {
		return nil
	}
	return &commandError{
		code:    protocol.CodeGitRepositoryInvalid,
		stage:   protocol.StageWorkspaceCheck,
		message: "受管仓库尚未就绪",
		details: map[string]any{"reason": check.Reason},
		cause:   errors.New("workspace is not healthy"),
	}
}

func requireUVReady(ctx context.Context, service environmentService, line uv.LineFunc) error {
	ready, err := checkM5UV(ctx, service, line)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	return &commandError{
		code:    protocol.CodeUVVersionMismatch,
		stage:   protocol.StageUVCheck,
		message: "固定版本 uv 尚未就绪",
		details: map[string]any{"uvVersion": uv.FixedVersion},
		cause:   errors.New("managed uv is not ready"),
	}
}

func dependencyPythonRequest(deps *deps, check gitrepo.CheckResult) uv.PythonRequest {
	return uv.PythonRequest{
		ProjectDir:       deps.global.layout.RepoDir(),
		PythonInstallDir: deps.global.layout.PythonDir(),
		ProjectEnvDir:    deps.global.layout.VenvDir(),
		CacheDir:         deps.global.layout.UVCacheDir(),
		Branch:           check.Branch,
		Commit:           check.Commit,
		MirrorPolicy:     deps.global.mirrorPolicy,
	}
}

func dependencyRequest(
	deps *deps,
	emitter *protocol.Emitter,
	check gitrepo.CheckResult,
	pythonVersion string,
	line uv.LineFunc,
) uv.DependenciesRequest {
	return uv.DependenciesRequest{
		ProjectDir:    deps.global.layout.RepoDir(),
		ProjectEnvDir: deps.global.layout.VenvDir(),
		PythonVersion: pythonVersion,
		OperationID:   emitter.OperationID(),
		Branch:        check.Branch,
		Commit:        check.Commit,
		MirrorPolicy:  deps.global.mirrorPolicy,
		Line:          line,
	}
}

func stageForDependencyAction(rebuild bool) protocol.Stage {
	if rebuild {
		return protocol.StageDependenciesRebuild
	}
	return protocol.StageDependenciesSync
}
