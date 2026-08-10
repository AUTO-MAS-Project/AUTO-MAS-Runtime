package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// environmentGroup 注册运行环境的检查、准备和修复命令。
func environmentGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "environment", Short: "运行环境操作"}
	group.AddCommand(
		environmentCheckCommand(deps),
		environmentEnsureCommand(deps),
		environmentRepairCommand(deps),
	)
	return group
}

func environmentCheckCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "只读检查 uv 与受管 Python",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageUVCheck,
				[]string{string(protocol.CapabilityStdinCancel)},
				func(ctx context.Context, _ *protocol.Emitter) (sessionSuccess, error) {
					service, err := deps.options.environmentFactory(deps.global.layout)
					if err != nil {
						return sessionSuccess{}, err
					}
					ready, err := checkM5UV(ctx, service, nil)
					if err != nil {
						return sessionSuccess{}, err
					}
					details := map[string]any{"uvReady": ready}
					if !ready {
						return sessionSuccess{message: "环境检查完成", details: details}, nil
					}
					python, err := service.CheckPython(ctx, uv.PythonRequest{
						ProjectDir:       deps.global.layout.RepoDir(),
						PythonInstallDir: deps.global.layout.PythonDir(),
						ProjectEnvDir:    deps.global.layout.VenvDir(),
						CacheDir:         deps.global.layout.UVCacheDir(),
						MirrorPolicy:     deps.global.mirrorPolicy,
						Line:             nil,
					})
					if err != nil {
						return sessionSuccess{}, err
					}
					details["pythonVersion"] = python.Spec.Version.String()
					return sessionSuccess{message: "环境检查完成", details: details}, nil
				},
			)
			return nil
		},
	}
}

func environmentEnsureCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "ensure",
		Short: "准备固定版本 uv",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageUVCheck,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (success sessionSuccess, returnErr error) {
					store, err := deps.options.environmentStateStoreFactory(ctx, deps.global.layout, deps.options.clock)
					if err != nil {
						return sessionSuccess{}, stateStoreError(protocol.StageUVCheck, err)
					}
					defer func() {
						if closeErr := store.Close(); closeErr != nil {
							returnErr = errors.Join(returnErr, stateStoreError(protocol.StageWorkspaceCleanup, closeErr))
						}
					}()
					service, err := deps.options.environmentFactory(deps.global.layout)
					if err != nil {
						return sessionSuccess{}, err
					}
					logger, err := openM5Logger(ctx, deps, "environment ensure", emitter.OperationID())
					if err != nil {
						return sessionSuccess{}, err
					}
					defer func() {
						if closeErr := logger.Close(); closeErr != nil {
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
					transaction, err := beginM5Transaction(
						ctx,
						store,
						emitter.OperationID(),
						"environment ensure",
						"",
						protocol.StageUVCheck,
					)
					if err != nil {
						return sessionSuccess{}, err
					}
					transactionActive := true
					defer func() {
						if !transactionActive || retainM5TransactionOnFailure(returnErr) {
							return
						}
						cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
						defer cancelCleanup()
						if cleanupErr := removeM5Transaction(cleanupContext, store, transaction); cleanupErr != nil {
							returnErr = errors.Join(cleanupErr, returnErr)
						}
					}()
					if err := transitionM5State(emitter, machine, protocol.StageUVCheck, protocol.StatePreparingUV, "正在准备固定版本 uv"); err != nil {
						return sessionSuccess{}, err
					}
					if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageUVDownload); err != nil {
						return sessionSuccess{}, err
					}
					if err := emitM5Progress(emitter, protocol.StageUVDownload, protocol.ProgressRunning, "正在准备固定版本 uv"); err != nil {
						return sessionSuccess{}, err
					}
					executable, err := ensureM5UV(
						ctx,
						service,
						emitter.OperationID(),
						deps.global.mirrorPolicy,
						uvLogLine(logger),
					)
					if err != nil {
						revision, _ := activeEnvironmentRevision(initial)
						return sessionSuccess{}, persistM5FailureWithLifecycle(
							ctx,
							emitter,
							store,
							deps.global.layout,
							initial,
							revision,
							"",
							uv.PythonSpec{},
							logger,
							machine,
							protocol.StageUVDownload,
							err,
						)
					}
					if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageUVVerify); err != nil {
						return sessionSuccess{}, err
					}
					if err := emitM5Progress(emitter, protocol.StageUVVerify, protocol.ProgressSucceeded, "固定版本 uv 已就绪"); err != nil {
						return sessionSuccess{}, err
					}
					cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
					removeErr := removeM5Transaction(cleanupContext, store, transaction)
					cancelCleanup()
					if removeErr != nil {
						return sessionSuccess{}, removeErr
					}
					transactionActive = false
					if err := rollbackM5Preparation(emitter, machine, protocol.StageUVVerify, "uv 环境准备完成，生命周期保持原稳定状态"); err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "uv 环境准备完成",
						details: map[string]any{"uvExecutable": executable, "uvVersion": uv.FixedVersion},
					}, nil
				},
			)
			return nil
		},
	}
}

func environmentRepairCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "repair",
		Short: "重新下载并校验 uv 与受管 Python，不重建项目 venv",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageRepair,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					return runEnvironmentRepair(ctx, deps, emitter)
				},
			)
			return nil
		},
	}
}

func runEnvironmentRepair(
	ctx context.Context,
	deps *deps,
	emitter *protocol.Emitter,
) (success sessionSuccess, returnErr error) {
	store, err := deps.options.environmentStateStoreFactory(ctx, deps.global.layout, deps.options.clock)
	if err != nil {
		return sessionSuccess{}, stateStoreError(protocol.StageRepair, err)
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
		"environment repair",
		check.Version,
		protocol.StageUVCheck,
	)
	if err != nil {
		return sessionSuccess{}, err
	}
	transactionActive := true
	defer func() {
		if !transactionActive || retainM5TransactionOnFailure(returnErr) {
			return
		}
		cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
		defer cancelCleanup()
		if cleanupErr := removeM5Transaction(cleanupContext, store, transaction); cleanupErr != nil {
			returnErr = errors.Join(cleanupErr, returnErr)
		}
	}()
	logger, err := openM5Logger(ctx, deps, "environment repair", emitter.OperationID())
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
	if err := transitionM5State(emitter, machine, protocol.StageUVCheck, protocol.StatePreparingUV, "正在修复固定版本 uv"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageUVDownload); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageUVDownload, protocol.ProgressRunning, "正在重新下载固定版本 uv"); err != nil {
		return sessionSuccess{}, err
	}
	uvExecutable, err := repairM5UV(
		ctx,
		service,
		emitter.OperationID(),
		deps.global.mirrorPolicy,
		uvLogLine(logger),
	)
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(
			ctx,
			emitter,
			store,
			deps.global.layout,
			initial,
			revision,
			"",
			uv.PythonSpec{},
			logger,
			machine,
			protocol.StageUVDownload,
			err,
		)
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageUVVerify); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StageUVVerify, protocol.ProgressSucceeded, "固定版本 uv 已重新校验"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StagePythonCheck); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StagePythonCheck, protocol.ProgressRunning, "正在读取项目 Python 契约"); err != nil {
		return sessionSuccess{}, err
	}
	pythonSpec, err := service.ReadPythonSpec(ctx, deps.global.layout.RepoDir())
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(
			ctx,
			emitter,
			store,
			deps.global.layout,
			initial,
			revision,
			uvExecutable,
			uv.PythonSpec{},
			logger,
			machine,
			protocol.StagePythonCheck,
			err,
		)
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StagePythonInstall); err != nil {
		return sessionSuccess{}, err
	}
	if err := emitM5Progress(emitter, protocol.StagePythonInstall, protocol.ProgressRunning, "正在重装受管 Python"); err != nil {
		return sessionSuccess{}, err
	}
	pythonResult, err := service.PreparePython(ctx, uv.PythonRequest{
		ProjectDir:       deps.global.layout.RepoDir(),
		ProjectEnvDir:    deps.global.layout.VenvDir(),
		PythonInstallDir: deps.global.layout.PythonDir(),
		CacheDir:         deps.global.layout.UVCacheDir(),
		Branch:           check.Branch,
		Commit:           check.Commit,
		MirrorPolicy:     deps.global.mirrorPolicy,
		Reinstall:        true,
		Line:             uvLogLine(logger),
	})
	if err != nil {
		return sessionSuccess{}, persistM5FailureWithLifecycle(
			ctx,
			emitter,
			store,
			deps.global.layout,
			initial,
			revision,
			uvExecutable,
			pythonSpec,
			logger,
			machine,
			protocol.StagePythonInstall,
			err,
		)
	}
	if err := emitM5Progress(emitter, protocol.StagePythonInstall, protocol.ProgressSucceeded, "受管 Python 已重装"); err != nil {
		return sessionSuccess{}, err
	}
	cleanupContext, cancelCleanup := m5TransactionCleanupContext(ctx)
	removeErr := removeM5Transaction(cleanupContext, store, transaction)
	cancelCleanup()
	if removeErr != nil {
		return sessionSuccess{}, removeErr
	}
	transactionActive = false
	if err := rollbackM5Preparation(emitter, machine, protocol.StagePythonInstall, "uv 与受管 Python 已修复，生命周期保持原稳定状态"); err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "uv 与受管 Python 修复完成",
		details: map[string]any{
			"uvExecutable":     uvExecutable,
			"uvVersion":        uv.FixedVersion,
			"pythonVersion":    pythonResult.Spec.Version.String(),
			"environmentState": string(initial.Status),
		},
	}, nil
}
