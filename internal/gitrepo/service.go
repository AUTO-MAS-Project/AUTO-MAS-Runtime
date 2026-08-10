package gitrepo

import (
	"context"
	"errors"
	"os"
	"reflect"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const serviceCleanupTimeout = 30 * time.Second

var (
	errInvalidService        = errors.New("workspace service is invalid")
	errInvalidServiceRequest = errors.New("workspace sync request is invalid")
)

// Service 是 workspace check/sync 的应用服务；check 不创建任何运行时能力。
type Service struct {
	layout     *config.Layout
	reader     repositoryReader
	newLocks   lockFactory
	newRuntime runtimeFactory
	buildPlan  planBuilder
}

// WorkspaceEmitter 是同步服务使用的最小协议事件出口。
type WorkspaceEmitter interface {
	EmitProgress(protocol.ProgressEvent) error
	EmitState(protocol.StateEvent) error
}

// OperationLogger 是同步操作所需的日志生命周期能力。
type OperationLogger interface {
	LogPath() string
	Close() error
}

// LoggerFactory 延迟创建本次同步的 Runtime logger，确保 check 保持只读。
type LoggerFactory func(ctx context.Context, command, operationID string) (OperationLogger, error)

// StageReporter 让 CLI 的 stdin 控制 warning 使用当前真实 stage。
type StageReporter func(protocol.Stage)

// SyncRequest 描述一次 workspace sync 的外部依赖和稳定身份。
type SyncRequest struct {
	Target           Target
	Policy           mirror.Policy
	OperationID      string
	PID              uint32
	MutationLease    MutationLease
	Emitter          WorkspaceEmitter
	LoggerFactory    LoggerFactory
	Auditor          filesystem.Auditor
	Clock            func() time.Time
	StageReporter    StageReporter
	ControlCommandID func() string
}

// CheckResult 是 workspace check 的稳定业务结果，不携带路径或 remote URL。
type CheckResult struct {
	Healthy           bool
	Version           string
	Branch            string
	Commit            string
	Source            string
	Reason            string
	directoryIdentity *filesystem.DirectoryIdentity
}

// SyncResult 是 workspace sync 的稳定业务结果。
type SyncResult struct {
	Revision         Revision
	Changed          bool
	Status           protocol.StateStatus
	ControlCommandID string
}

type lockFactory func(context.Context, *config.Layout) (mutationLockSet, error)

type mutationLockSet interface {
	AcquireMutation(context.Context) (mutationLease, error)
	Close() error
}

// MutationLease 是跨 workspace 与环境编排共享的 mutation 锁租约。
type MutationLease interface {
	Close() error
}

type mutationLease = MutationLease

// MutationCoordinator 提供一次受管 mutation 操作的锁生命周期。
type MutationCoordinator interface {
	AcquireMutation(context.Context) (MutationLease, error)
	Close() error
}

type planBuilder func(mirror.Policy) (mirror.Plan, error)

type runtimeFactory func(
	context.Context,
	*config.Layout,
	SyncRequest,
	OperationLogger,
) (syncRuntime, error)

type syncRuntime interface {
	Recover(context.Context, RecoveryRequest) (RecoveryResult, error)
	ReadEnvironment(context.Context) (state.EnvironmentState, error)
	NewTransaction(state.TransactionKind, state.TransactionInput) (state.TransactionState, error)
	WriteTransaction(context.Context, state.TransactionKind, state.TransactionState) error
	ReadTransaction(context.Context, state.TransactionKind) (state.TransactionSnapshot, error)
	RemoveTransaction(context.Context, state.TransactionSnapshot) error
	Fetch(context.Context, FetchRequest) (FetchResult, error)
	Swap(context.Context, SwapRequest) (SwapResult, error)
	NewBrokenEnvironment(state.Revision, state.BrokenEnvironment) (state.EnvironmentState, error)
	WriteEnvironment(context.Context, state.EnvironmentState) error
	Close() error
}

// NewService 创建使用生产 go-git、filesystem、state 和 Windows Mutex 的服务。
func NewService(layout *config.Layout) (*Service, error) {
	return newServiceWithDependencies(
		layout,
		goGitRepositoryReader{},
		newProductionLocks,
		newProductionRuntime,
		buildProductionPlan,
	)
}

// NewMutationCoordinator 创建可由上层编排持有的生产 mutation 锁协调器。
func NewMutationCoordinator(ctx context.Context, layout *config.Layout) (MutationCoordinator, error) {
	return newProductionLocks(ctx, layout)
}

func newServiceWithDependencies(
	layout *config.Layout,
	reader repositoryReader,
	newLocks lockFactory,
	newRuntime runtimeFactory,
	buildPlan planBuilder,
) (*Service, error) {
	if layout == nil || nilDependency(reader) || newLocks == nil ||
		newRuntime == nil || buildPlan == nil {
		return nil, errInvalidService
	}
	return &Service{
		layout:     layout,
		reader:     reader,
		newLocks:   newLocks,
		newRuntime: newRuntime,
		buildPlan:  buildPlan,
	}, nil
}

// Check 只读取 app-root/repo 及其 Git 对象，不创建目录、状态、锁、日志或网络请求。
func (s *Service) Check(ctx context.Context) (CheckResult, error) {
	if ctx == nil || s == nil || s.layout == nil || nilDependency(s.reader) {
		return CheckResult{}, serviceInternalError(protocol.StageWorkspaceCheck, errInvalidService)
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	root, err := filesystem.InspectManagedDirectory(ctx, s.layout, s.layout.AppRoot())
	if isCancellation(ctx, err) {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if errors.Is(err, filesystem.ErrIdentityChanged) || isUnsafeManagedPath(err) {
		return CheckResult{Reason: "invalid"}, nil
	}
	if err != nil {
		return CheckResult{}, serviceCheckReadError(err)
	}
	if !root.Exists {
		return CheckResult{Reason: "missing"}, nil
	}
	repository, err := filesystem.InspectManagedDirectory(ctx, s.layout, s.layout.RepoDir())
	if isCancellation(ctx, err) {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if errors.Is(err, filesystem.ErrIdentityChanged) || isUnsafeManagedPath(err) {
		return CheckResult{Reason: "invalid"}, nil
	}
	if err != nil {
		return CheckResult{}, serviceCheckReadError(err)
	}
	if !repository.Exists {
		return CheckResult{Reason: "missing"}, nil
	}

	lease, err := filesystem.PinManagedDirectory(ctx, s.layout, s.layout.RepoDir())
	if isCancellation(ctx, err) {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if errors.Is(err, filesystem.ErrIdentityChanged) || isUnsafeManagedPath(err) {
		return CheckResult{Reason: "invalid"}, nil
	}
	if err != nil {
		return CheckResult{}, serviceCheckReadError(err)
	}
	if lease == nil {
		return CheckResult{}, serviceCheckReadError(errDirectoryIdentityMissing)
	}
	directoryIdentity := lease.Identity()
	if directoryIdentity == nil {
		closeErr := lease.Close()
		return CheckResult{}, serviceCheckReadError(errors.Join(errDirectoryIdentityMissing, closeErr))
	}
	if repository.Identity == nil || !repository.Identity.Equal(directoryIdentity) {
		closeErr := lease.Close()
		if closeErr != nil {
			return CheckResult{}, serviceCheckReadError(errors.Join(filesystem.ErrIdentityChanged, closeErr))
		}
		return CheckResult{Reason: "invalid"}, nil
	}
	snapshot, inspectErr := s.reader.Inspect(ctx, s.layout.RepoDir())
	closeErr := lease.Close()
	if closeErr != nil {
		inspectErr = errors.Join(inspectErr, closeErr)
	}
	err = inspectErr
	if closeErr != nil {
		if isCancellation(ctx, err) {
			return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
		}
		return CheckResult{}, serviceCheckReadError(err)
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if isCancellation(ctx, err) {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return CheckResult{}, serviceCheckReadError(err)
		}
		return CheckResult{Reason: "invalid", directoryIdentity: directoryIdentity}, nil
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	if reason := checkSnapshotReason(snapshot); reason != "" {
		return CheckResult{Reason: reason, directoryIdentity: directoryIdentity}, nil
	}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil {
		return CheckResult{
			Reason:            checkSnapshotReason(snapshot),
			directoryIdentity: directoryIdentity,
		}, nil
	}
	return CheckResult{
		Healthy:           true,
		Version:           identity.version,
		Branch:            identity.branch,
		Commit:            identity.commit,
		Source:            identity.sourceKey,
		Reason:            "ok",
		directoryIdentity: directoryIdentity,
	}, nil
}

// Sync 按冻结顺序执行锁协调、恢复、Git 获取、整体替换和环境失效持久化。
func (s *Service) Sync(ctx context.Context, request SyncRequest) (result SyncResult, returnErr error) {
	if ctx == nil || s == nil || s.layout == nil || nilDependency(s.reader) ||
		s.newLocks == nil || s.newRuntime == nil || s.buildPlan == nil {
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceClone, errInvalidServiceRequest)
	}
	if err := validateSyncRequest(request); err != nil {
		return SyncResult{}, err
	}
	if request.Clock == nil {
		request.Clock = time.Now
	}
	if err := ctx.Err(); err != nil {
		return SyncResult{}, serviceCancelledError(protocol.StageWorkspaceClone, err)
	}
	if err := requireExistingDirectory(ctx, s.layout); err != nil {
		if isCancellation(ctx, err) {
			return SyncResult{}, serviceCancelledError(protocol.StageWorkspaceClone, err)
		}
		if code, ok := filesystemOperationCode(err); ok {
			return SyncResult{}, newError(code, protocol.StageWorkspaceClone, messageForCode(code), map[string]any{}, err)
		}
		return SyncResult{}, serviceInvalidArgumentError(err)
	}
	plan, err := s.buildPlan(request.Policy)
	if err != nil {
		if errors.Is(err, mirror.ErrPolicyRejected) {
			return SyncResult{}, servicePolicyArgumentError(err)
		}
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceClone, err)
	}

	logger, err := request.LoggerFactory(ctx, "workspace-sync", request.OperationID)
	if err != nil || nilOperationLogger(logger) {
		if err == nil {
			err = errInvalidServiceRequest
		}
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceClone, err)
	}
	var locks mutationLockSet
	var lease mutationLease
	leaseOwned := request.MutationLease == nil
	var runtime syncRuntime
	defer func() {
		returnErr = joinServiceCloseError(returnErr, runtime, lease, locks, logger, leaseOwned)
	}()

	if request.MutationLease != nil {
		lease = request.MutationLease
	} else {
		locks, err = s.newLocks(ctx, s.layout)
		if err != nil || nilMutationLockSet(locks) {
			if err == nil {
				err = errInvalidService
			}
			return SyncResult{}, mapMutexFailure(err)
		}
		acquired, acquireErr := locks.AcquireMutation(ctx)
		if acquireErr != nil || nilMutationLease(acquired) {
			if acquireErr == nil {
				acquireErr = errInvalidService
			}
			return SyncResult{}, mapMutexFailure(acquireErr)
		}
		lease = acquired
	}

	runtime, err = s.newRuntime(ctx, s.layout, request, logger)
	if err != nil || nilSyncRuntime(runtime) {
		if err == nil {
			err = errInvalidService
		}
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceClone, err)
	}

	setStage(request, protocol.StageWorkspaceCleanup)
	_, err = runtime.Recover(ctx, RecoveryRequest{
		LogPath:       logger.LogPath(),
		StageReporter: request.StageReporter,
	})
	if err != nil {
		return SyncResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SyncResult{}, s.cancelBeforeSwap(ctx, request, runtime, err, protocol.StageWorkspaceCleanup)
	}
	environment, initialStatus, err := readStableEnvironment(ctx, runtime)
	if err != nil {
		return SyncResult{}, err
	}
	machine, err := protocol.NewLifecycleMachine(initialStatus)
	if err != nil {
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceCheck, err)
	}
	if err := machine.Transition(protocol.StateSyncingRepository); err != nil {
		return SyncResult{}, serviceInternalError(protocol.StageWorkspaceCheck, err)
	}
	setStage(request, protocol.StageWorkspaceCheck)
	if err := emitState(request, protocol.StageWorkspaceCheck, protocol.StateSyncingRepository, "正在同步后端仓库", map[string]any{
		"version": request.Target.Version(),
		"branch":  request.Target.Branch(),
	}); err != nil {
		return SyncResult{}, err
	}

	mutation, err := runtime.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID:   request.OperationID,
		Command:       "workspace sync",
		PID:           request.PID,
		TargetVersion: request.Target.Version(),
		Stage:         protocol.StageWorkspaceClone,
	})
	if err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, serviceStateWriteError(protocol.StageWorkspaceClone, err))
	}
	if err := runtime.WriteTransaction(ctx, state.TransactionMutation, mutation); err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, mapStateWriteError(protocol.StageWorkspaceClone, err))
	}

	check, err := s.Check(ctx)
	if err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
	}
	if check.Healthy && check.Version == request.Target.Version() &&
		check.Branch == request.Target.Branch() {
		setStage(request, protocol.StageWorkspaceCleanup)
		if err := removeTransaction(ctx, runtime, state.TransactionMutation); err != nil {
			return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
		}
		if err := machine.RollbackPreparation(); err != nil {
			return SyncResult{}, serviceInternalError(protocol.StageWorkspaceCheck, err)
		}
		setStage(request, protocol.StageWorkspaceCheck)
		if err := emitState(request, protocol.StageWorkspaceCheck, initialStatus, "后端仓库已是目标版本", map[string]any{
			"version": request.Target.Version(),
			"branch":  request.Target.Branch(),
		}); err != nil {
			return SyncResult{}, err
		}
		return SyncResult{
			Revision: Revision{
				version:   check.Version,
				branch:    check.Branch,
				commit:    check.Commit,
				sourceKey: check.Source,
			},
			Changed:          false,
			Status:           initialStatus,
			ControlCommandID: controlCommandID(request),
		}, nil
	}

	update, err := runtime.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID:   request.OperationID,
		Command:       "workspace sync",
		PID:           request.PID,
		TargetVersion: request.Target.Version(),
		Stage:         protocol.StageWorkspaceClone,
	})
	if err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, serviceStateWriteError(protocol.StageWorkspaceClone, err))
	}
	if err := runtime.WriteTransaction(ctx, state.TransactionUpdate, update); err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, mapStateWriteError(protocol.StageWorkspaceClone, err))
	}
	setStage(request, protocol.StageWorkspaceClone)
	if err := advanceTransactions(ctx, runtime, &mutation, &update, protocol.StageWorkspaceClone); err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
	}

	fetched, err := runtime.Fetch(ctx, FetchRequest{
		Plan:          plan,
		Target:        request.Target,
		OperationID:   request.OperationID,
		StageReporter: request.StageReporter,
	})
	if err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
	}
	setStage(request, protocol.StageWorkspaceVerify)
	if err := advanceTransactions(ctx, runtime, &mutation, &update, protocol.StageWorkspaceVerify); err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
	}

	setStage(request, protocol.StageWorkspaceSwap)
	environmentCommitted := false
	swapResult, swapErr := runtime.Swap(ctx, SwapRequest{
		Transaction:    update,
		Revision:       fetched.Revision,
		ActiveIdentity: check.directoryIdentity,
		UpdateIdentity: fetched.DirectoryIdentity,
		StageReporter:  request.StageReporter,
		CommitEnvironment: func(commitCtx context.Context, revision Revision) error {
			setStage(request, protocol.StageWorkspaceCleanup)
			if err := advanceMutation(commitCtx, runtime, &mutation, protocol.StageWorkspaceCleanup); err != nil {
				return serviceCommittedStateWriteError(protocol.StageWorkspaceCleanup, err)
			}
			setStage(request, protocol.StageWorkspaceSwap)
			broken, err := runtime.NewBrokenEnvironment(environment.LastSuccessful, state.BrokenEnvironment{
				TargetVersion: revision.Version(),
				Branch:        revision.Branch(),
				Commit:        revision.Commit(),
				Reason:        state.ReasonRepositoryChanged,
				Stage:         protocol.StageWorkspaceSwap,
				ExitCode:      0,
				LogPath:       logger.LogPath(),
			})
			if err != nil {
				return serviceCommittedStateWriteError(protocol.StageWorkspaceSwap, err)
			}
			if err := runtime.WriteEnvironment(commitCtx, broken); err != nil {
				return serviceCommittedStateWriteError(protocol.StageWorkspaceSwap, err)
			}
			if err := machine.Transition(protocol.StateEnvironmentBroken); err != nil {
				return serviceCommittedInternalError(protocol.StageWorkspaceSwap, err)
			}
			if err := emitState(request, protocol.StageWorkspaceSwap, protocol.StateEnvironmentBroken, "后端仓库已更新，主环境需要重新准备", map[string]any{
				"version": revision.Version(),
				"branch":  revision.Branch(),
				"commit":  revision.Commit(),
			}); err != nil {
				return serviceCommittedOperationError(err)
			}
			environmentCommitted = true
			return nil
		},
	})
	active := swapResult.RepositoryActivated
	if !active {
		if swapErr == nil {
			swapErr = serviceInternalError(protocol.StageWorkspaceSwap, errors.New("repository swap did not activate target"))
		}
		if swapResult.MutationApplied {
			if recoveryErr := s.recoverPartialSwap(ctx, request, runtime, logger.LogPath()); recoveryErr != nil {
				return SyncResult{}, errors.Join(recoveryErr, swapErr)
			}
		}
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, swapErr)
	}
	if swapErr != nil {
		return SyncResult{}, swapErr
	}
	if !environmentCommitted {
		return SyncResult{}, serviceCommittedInternalError(
			protocol.StageWorkspaceSwap,
			errors.New("repository swap skipped environment commit callback"),
		)
	}
	commitContext, cancelCommit := serviceCleanupContext(ctx)
	defer cancelCommit()
	setStage(request, protocol.StageWorkspaceCleanup)
	if err := removeTransaction(commitContext, runtime, state.TransactionUpdate); err != nil {
		return SyncResult{}, serviceCommittedStateWriteError(protocol.StageWorkspaceCleanup, err)
	}
	if err := removeTransaction(commitContext, runtime, state.TransactionMutation); err != nil {
		return SyncResult{}, serviceCommittedStateWriteError(protocol.StageWorkspaceCleanup, err)
	}
	return SyncResult{
		Revision:         fetched.Revision,
		Changed:          true,
		Status:           protocol.StateEnvironmentBroken,
		ControlCommandID: controlCommandID(request),
	}, nil
}

func (s *Service) finishPreSwap(
	ctx context.Context,
	request SyncRequest,
	runtime syncRuntime,
	machine *protocol.LifecycleMachine,
	cause error,
) error {
	setStage(request, protocol.StageWorkspaceCleanup)
	if machine != nil {
		if rollbackErr := machine.RollbackPreparation(); rollbackErr == nil {
			if emitErr := emitState(request, protocol.StageWorkspaceCleanup, machine.Initial(), "仓库同步未改变当前工作区", map[string]any{}); emitErr != nil {
				cause = errors.Join(cause, emitErr)
			}
		}
	}
	cleanupContext, cancel := serviceCleanupContext(ctx)
	defer cancel()
	if !preserveUpdateTransaction(cleanupContext, s.layout, request.OperationID, cause) {
		if cleanupErr := removeTransaction(cleanupContext, runtime, state.TransactionUpdate); cleanupErr != nil {
			cause = errors.Join(cause, cleanupErr)
		}
	}
	if cleanupErr := removeTransaction(cleanupContext, runtime, state.TransactionMutation); cleanupErr != nil {
		cause = errors.Join(cause, cleanupErr)
	}
	return cause
}

func (s *Service) recoverPartialSwap(
	ctx context.Context,
	request SyncRequest,
	runtime syncRuntime,
	logPath string,
) error {
	cleanupContext, cancel := serviceCleanupContext(ctx)
	defer cancel()
	setStage(request, protocol.StageWorkspaceCleanup)
	result, err := runtime.Recover(cleanupContext, RecoveryRequest{
		LogPath:       logPath,
		StageReporter: request.StageReporter,
	})
	if err != nil {
		return err
	}
	if !result.Recovered || !result.TransactionRemoved {
		return serviceInternalError(protocol.StageWorkspaceCleanup, errors.New("partial swap recovery did not complete"))
	}
	return nil
}

func (s *Service) cancelBeforeSwap(
	ctx context.Context,
	request SyncRequest,
	runtime syncRuntime,
	cause error,
	stage protocol.Stage,
) error {
	cleanupContext, cancel := serviceCleanupContext(ctx)
	defer cancel()
	cleanupErr := errors.Join(
		removeTransaction(cleanupContext, runtime, state.TransactionUpdate),
		removeTransaction(cleanupContext, runtime, state.TransactionMutation),
	)
	return errors.Join(
		serviceCancelledErrorWithDetails(stage, cause, controlDetails(request)),
		cleanupErr,
	)
}

func serviceCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), serviceCleanupTimeout)
}

func joinPrimaryError(primary, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return errors.Join(primary, secondary)
}

func readStableEnvironment(
	ctx context.Context,
	runtime syncRuntime,
) (state.EnvironmentState, protocol.StateStatus, error) {
	environment, err := runtime.ReadEnvironment(ctx)
	if err == nil {
		return environment, environment.Status, nil
	}
	if errors.Is(err, state.ErrNotFound) {
		return state.EnvironmentState{}, protocol.StateUninitialized, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return state.EnvironmentState{}, "", serviceCancelledError(protocol.StageWorkspaceCheck, err)
	}
	return state.EnvironmentState{}, "", serviceAmbiguousError(protocol.StageWorkspaceCheck, "environment_unreadable", err)
}

func advanceTransactions(
	ctx context.Context,
	runtime syncRuntime,
	mutation *state.TransactionState,
	update *state.TransactionState,
	stage protocol.Stage,
) error {
	mutation.Stage = stage
	if err := runtime.WriteTransaction(ctx, state.TransactionMutation, *mutation); err != nil {
		return mapStateWriteError(stage, err)
	}
	update.Stage = stage
	if err := runtime.WriteTransaction(ctx, state.TransactionUpdate, *update); err != nil {
		return mapStateWriteError(stage, err)
	}
	return nil
}

func advanceMutation(
	ctx context.Context,
	runtime syncRuntime,
	mutation *state.TransactionState,
	stage protocol.Stage,
) error {
	mutation.Stage = stage
	if err := runtime.WriteTransaction(ctx, state.TransactionMutation, *mutation); err != nil {
		return mapStateWriteError(stage, err)
	}
	return nil
}

func removeTransaction(
	ctx context.Context,
	runtime syncRuntime,
	kind state.TransactionKind,
) error {
	snapshot, err := runtime.ReadTransaction(ctx, kind)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapStateWriteError(protocol.StageWorkspaceCleanup, err)
	}
	if err := runtime.RemoveTransaction(ctx, snapshot); err != nil {
		return mapStateWriteError(protocol.StageWorkspaceCleanup, err)
	}
	return nil
}

func preserveUpdateTransaction(
	ctx context.Context,
	layout *config.Layout,
	operationID string,
	cause error,
) bool {
	if layout == nil || operationID == "" {
		return true
	}
	path, err := layout.RepoUpdateDir(operationID)
	if err != nil {
		return true
	}
	inspection, err := filesystem.InspectManagedDirectory(ctx, layout, path)
	if err != nil {
		return true
	}
	if inspection.Exists {
		return true
	}
	var operationErr *Error
	if errors.As(cause, &operationErr) {
		switch operationErr.Code() {
		case protocol.CodeUpdateStateAmbiguous, protocol.CodeStateWriteFailed,
			protocol.CodeGitRepoSwapFailed, protocol.CodeGitRepoCleanupFailed:
			return true
		}
	}
	return false
}

func validateSyncRequest(request SyncRequest) error {
	if request.Target.validate() != nil {
		return serviceInvalidVersionError()
	}
	if request.OperationID == "" || request.PID == 0 ||
		nilWorkspaceEmitter(request.Emitter) || request.LoggerFactory == nil ||
		nilInterface(request.Auditor) ||
		(request.MutationLease != nil && nilMutationLease(request.MutationLease)) {
		return serviceInternalError(protocol.StageWorkspaceClone, errInvalidServiceRequest)
	}
	return nil
}

func requireExistingDirectory(ctx context.Context, layout *config.Layout) error {
	inspection, err := filesystem.InspectManagedDirectory(ctx, layout, layout.AppRoot())
	if err != nil {
		return err
	}
	if !inspection.Exists {
		return errors.New("app root does not exist")
	}
	return nil
}

func isUnsafeManagedPath(err error) bool {
	var coded interface{ Code() protocol.Code }
	if errors.As(err, &coded) {
		switch coded.Code() {
		case protocol.CodeUnsafeReparsePoint, protocol.CodePathOutsideManagedRoot:
			return true
		}
	}
	return errors.Is(err, filesystem.ErrUnsupportedCaseSensitivity)
}

func filesystemOperationCode(err error) (protocol.Code, bool) {
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) {
		return "", false
	}
	switch coded.Code() {
	case protocol.CodePathOutsideManagedRoot, protocol.CodeUnsafeReparsePoint:
		return coded.Code(), true
	default:
		return "", false
	}
}

func checkSnapshotReason(snapshot repositorySnapshot) string {
	if _, err := repositoryIdentityFromSnapshot(snapshot); err == nil {
		return ""
	}
	if len(snapshot.remotes) == 1 {
		remote := snapshot.remotes[0]
		if remote.name == "origin" && len(remote.fetchURLs) == 1 {
			if _, ok := recoverySourceForURL(remote.fetchURLs[0]); !ok {
				return "remote_unknown"
			}
		}
	}
	version, err := parseRepositoryVersion(snapshot.versionPayload)
	if err == nil {
		target, targetErr := ParseTarget(version)
		if targetErr == nil && (!snapshot.headSymbolic || snapshot.headTarget != "refs/heads/"+target.Branch()) {
			return "version_mismatch"
		}
	}
	return "invalid"
}

func emitState(
	request SyncRequest,
	stage protocol.Stage,
	status protocol.StateStatus,
	message string,
	details map[string]any,
) error {
	if details == nil {
		details = map[string]any{}
	}
	for key, value := range controlDetails(request) {
		details[key] = value
	}
	if err := request.Emitter.EmitState(protocol.StateEvent{
		Stage:   stage,
		Status:  status,
		Message: message,
		Details: details,
	}); err != nil {
		return serviceOutputError(stage, err)
	}
	return nil
}

func setStage(request SyncRequest, stage protocol.Stage) {
	if request.StageReporter != nil {
		request.StageReporter(stage)
	}
}

func controlCommandID(request SyncRequest) string {
	if request.ControlCommandID == nil {
		return ""
	}
	return request.ControlCommandID()
}

func controlDetails(request SyncRequest) map[string]any {
	commandID := controlCommandID(request)
	if commandID == "" {
		return map[string]any{}
	}
	return map[string]any{"controlCommandId": commandID}
}

func buildProductionPlan(policy mirror.Policy) (mirror.Plan, error) {
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return mirror.Plan{}, err
	}
	return mirror.BuildPlan(catalog, policy, mirror.KindGit)
}

type productionMutexSet struct{ set *lock.Set }

func newProductionLocks(ctx context.Context, layout *config.Layout) (mutationLockSet, error) {
	set, err := lock.NewSet(ctx, layout)
	if err != nil {
		return nil, err
	}
	return productionMutexSet{set: set}, nil
}

func (s productionMutexSet) AcquireMutation(ctx context.Context) (mutationLease, error) {
	result, err := s.set.AcquireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if result.Lease() == nil {
		return nil, errInvalidService
	}
	return result.Lease(), nil
}

func (s productionMutexSet) Close() error { return s.set.Close() }

type productionRuntime struct {
	store    *state.Store
	recovery *Recovery
	fetcher  *Fetcher
	swapper  *Swapper
}

func newProductionRuntime(
	ctx context.Context,
	layout *config.Layout,
	request SyncRequest,
	logger OperationLogger,
) (syncRuntime, error) {
	clock := request.Clock
	if clock == nil {
		clock = time.Now
	}
	store, err := state.NewStore(ctx, layout, state.WithClock(clock))
	if err != nil {
		return nil, err
	}
	operator, err := filesystem.New(ctx, layout, request.Auditor)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	recovery, err := NewRecovery(layout, operator, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	rotator, err := mirror.NewRotator()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	fetcher, err := NewFetcher(layout, rotator, operator, NewVerifier(), request.Emitter)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	swapper, err := NewSwapper(layout, operator, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if nilOperationLogger(logger) {
		_ = store.Close()
		return nil, errInvalidServiceRequest
	}
	return &productionRuntime{
		store:    store,
		recovery: recovery,
		fetcher:  fetcher,
		swapper:  swapper,
	}, nil
}

func (r *productionRuntime) Recover(ctx context.Context, request RecoveryRequest) (RecoveryResult, error) {
	return r.recovery.Recover(ctx, request)
}
func (r *productionRuntime) ReadEnvironment(ctx context.Context) (state.EnvironmentState, error) {
	return r.store.ReadEnvironment(ctx)
}
func (r *productionRuntime) NewTransaction(kind state.TransactionKind, input state.TransactionInput) (state.TransactionState, error) {
	return r.store.NewTransaction(kind, input)
}
func (r *productionRuntime) WriteTransaction(ctx context.Context, kind state.TransactionKind, value state.TransactionState) error {
	return r.store.WriteTransaction(ctx, kind, value)
}
func (r *productionRuntime) ReadTransaction(ctx context.Context, kind state.TransactionKind) (state.TransactionSnapshot, error) {
	return r.store.ReadTransaction(ctx, kind)
}
func (r *productionRuntime) RemoveTransaction(ctx context.Context, snapshot state.TransactionSnapshot) error {
	return r.store.RemoveTransaction(ctx, snapshot)
}
func (r *productionRuntime) Fetch(ctx context.Context, request FetchRequest) (FetchResult, error) {
	return r.fetcher.Fetch(ctx, request)
}
func (r *productionRuntime) Swap(ctx context.Context, request SwapRequest) (SwapResult, error) {
	return r.swapper.Swap(ctx, request)
}
func (r *productionRuntime) NewBrokenEnvironment(lastSuccessful state.Revision, broken state.BrokenEnvironment) (state.EnvironmentState, error) {
	return r.store.NewBrokenEnvironment(lastSuccessful, broken)
}
func (r *productionRuntime) WriteEnvironment(ctx context.Context, value state.EnvironmentState) error {
	return r.store.WriteEnvironment(ctx, value)
}
func (r *productionRuntime) Close() error { return r.store.Close() }

func serviceCheckReadError(cause error) *Error {
	return newError(protocol.CodeGitRepositoryInvalid, protocol.StageWorkspaceCheck, "无法读取受管后端仓库", map[string]any{}, cause)
}

func serviceInvalidArgumentError(cause error) *Error {
	return newError(protocol.CodeInvalidArgument, protocol.StageWorkspaceClone, "应用根目录无效", map[string]any{}, cause)
}

func servicePolicyArgumentError(cause error) *Error {
	return newError(protocol.CodeInvalidArgument, protocol.StageWorkspaceClone, "镜像策略参数无效", map[string]any{}, cause)
}

func serviceInvalidVersionError() *Error {
	return newError(protocol.CodeInvalidVersion, protocol.StageWorkspaceClone, messageForCode(protocol.CodeInvalidVersion), map[string]any{}, ErrInvalidVersion)
}

func serviceCancelledError(stage protocol.Stage, cause error) *Error {
	return serviceCancelledErrorWithDetails(stage, cause, nil)
}

func serviceCancelledErrorWithDetails(stage protocol.Stage, cause error, details map[string]any) *Error {
	return newError(protocol.CodeOperationCancelled, stage, messageForCode(protocol.CodeOperationCancelled), details, cause)
}

func serviceOutputError(stage protocol.Stage, cause error) *Error {
	return newError(protocol.CodeOutputWriteFailed, stage, messageForCode(protocol.CodeOutputWriteFailed), map[string]any{}, cause)
}

func serviceStateWriteError(stage protocol.Stage, cause error) *Error {
	return newError(protocol.CodeStateWriteFailed, stage, messageForCode(protocol.CodeStateWriteFailed), map[string]any{}, cause)
}

func serviceCommittedStateWriteError(stage protocol.Stage, cause error) *Error {
	return newCommittedError(protocol.CodeStateWriteFailed, stage, messageForCode(protocol.CodeStateWriteFailed), map[string]any{}, cause)
}

func serviceCommittedInternalError(stage protocol.Stage, cause error) *Error {
	return newCommittedError(protocol.CodeInternalError, stage, messageForCode(protocol.CodeInternalError), map[string]any{}, cause)
}

func serviceCommittedOperationError(cause error) *Error {
	var operationErr *Error
	if errors.As(cause, &operationErr) {
		return newCommittedError(
			operationErr.Code(),
			operationErr.Stage(),
			operationErr.Message(),
			operationErr.Details(),
			cause,
		)
	}
	return serviceCommittedInternalError(protocol.StageWorkspaceSwap, cause)
}

func mapStateWriteError(stage protocol.Stage, cause error) *Error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return serviceCancelledError(stage, cause)
	}
	return serviceStateWriteError(stage, cause)
}

func serviceAmbiguousError(stage protocol.Stage, reason string, cause error) *Error {
	return newError(protocol.CodeUpdateStateAmbiguous, stage, messageForCode(protocol.CodeUpdateStateAmbiguous), map[string]any{"reason": reason}, cause)
}

func serviceInternalError(stage protocol.Stage, cause error) *Error {
	return newError(protocol.CodeInternalError, stage, messageForCode(protocol.CodeInternalError), map[string]any{}, cause)
}

func mapMutexFailure(cause error) *Error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return serviceCancelledError(protocol.StageWorkspaceClone, cause)
	}
	var operation interface{ Code() protocol.Code }
	if errors.As(cause, &operation) {
		code := operation.Code()
		if code == protocol.CodeBackendStillRunning || code == protocol.CodeMutationInProgress || code == protocol.CodeMutexOperationFailed {
			return newError(code, protocol.StageWorkspaceClone, messageForCode(code), map[string]any{}, cause)
		}
	}
	return newError(protocol.CodeMutexOperationFailed, protocol.StageWorkspaceClone, messageForCode(protocol.CodeMutexOperationFailed), map[string]any{}, cause)
}

func joinServiceCloseError(
	current error,
	runtime syncRuntime,
	lease mutationLease,
	locks mutationLockSet,
	logger OperationLogger,
	closeLease bool,
) error {
	if runtime != nil {
		if err := runtime.Close(); err != nil {
			current = joinPrimaryError(serviceStateWriteError(protocol.StageWorkspaceCleanup, err), current)
		}
	}
	if lease != nil && closeLease {
		if err := lease.Close(); err != nil {
			current = joinPrimaryError(mapMutexFailure(err), current)
		}
	}
	if locks != nil {
		if err := locks.Close(); err != nil {
			current = joinPrimaryError(mapMutexFailure(err), current)
		}
	}
	if logger != nil {
		if err := logger.Close(); err != nil {
			current = joinPrimaryError(serviceInternalError(protocol.StageWorkspaceCleanup, err), current)
		}
	}
	return current
}

func nilOperationLogger(value OperationLogger) bool {
	return nilInterface(value)
}

func nilWorkspaceEmitter(value WorkspaceEmitter) bool { return nilInterface(value) }
func nilMutationLockSet(value mutationLockSet) bool   { return nilInterface(value) }
func nilMutationLease(value mutationLease) bool       { return nilInterface(value) }
func nilSyncRuntime(value syncRuntime) bool           { return nilInterface(value) }

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ mutationLockSet = productionMutexSet{}
var _ syncRuntime = (*productionRuntime)(nil)
