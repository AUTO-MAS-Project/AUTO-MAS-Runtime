package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func newM5RepairCommand(deps *deps, use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithStdinCancel(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageRepair,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (success sessionSuccess, returnErr error) {
					return runRepair(ctx, deps, emitter)
				},
			)
			return nil
		},
	}
}

func runRepair(
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
	if err := recoverM5Transaction(ctx, store); err != nil {
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
	if !check.Healthy {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeGitRepositoryInvalid,
			stage:   protocol.StageWorkspaceCheck,
			message: "受管仓库尚未就绪",
			details: map[string]any{"reason": check.Reason},
			cause:   errors.New("workspace is not healthy"),
		}
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
		"repair",
		check.Version,
		protocol.StageRepair,
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
		if cleanupErr := removeM5Transaction(cleanupContext, store); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	logger, err := openM5Logger(ctx, deps, "repair", emitter.OperationID())
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
	if err := transitionM5State(emitter, machine, protocol.StageRepair, protocol.StatePreparingUV, "正在检查并修复运行环境"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageUVCheck); err != nil {
		return sessionSuccess{}, err
	}
	if err := transitionM5State(emitter, machine, protocol.StagePythonCheck, protocol.StatePreparingPython, "正在准备受管 Python"); err != nil {
		return sessionSuccess{}, err
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StagePythonInstall); err != nil {
		return sessionSuccess{}, err
	}
	if err := transitionM5State(emitter, machine, protocol.StageDependenciesSync, protocol.StateSyncingEnvironment, "正在重建并同步项目依赖"); err != nil {
		return sessionSuccess{}, err
	}
	result, err := service.Repair(ctx, uv.EnvironmentRequest{
		ProjectDir:       deps.global.layout.RepoDir(),
		ProjectEnvDir:    deps.global.layout.VenvDir(),
		PythonInstallDir: deps.global.layout.PythonDir(),
		CacheDir:         deps.global.layout.UVCacheDir(),
		OperationID:      emitter.OperationID(),
		Branch:           check.Branch,
		Commit:           check.Commit,
		BootstrapPolicy:  deps.global.mirrorPolicy,
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
			"",
			uv.PythonSpec{},
			logger,
			machine,
			protocol.StagePythonCheck,
			err,
		)
	}
	if err := advanceM5Transaction(ctx, store, &transaction, protocol.StageDependenciesSync); err != nil {
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
	if err := transitionM5State(emitter, machine, protocol.StageDependenciesSync, protocol.StateReadyToStart, "运行环境修复完成"); err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "运行环境修复完成",
		status:  string(protocol.StateReadyToStart),
		details: map[string]any{
			"uvExecutable":  result.UVExecutable,
			"uvVersion":     uv.FixedVersion,
			"pythonVersion": result.Python.Version.String(),
			"synchronized":  result.Dependencies.Synchronized,
		},
	}, nil
}
