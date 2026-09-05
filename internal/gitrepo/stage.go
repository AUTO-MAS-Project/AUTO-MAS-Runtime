package gitrepo

import (
	"context"
	"errors"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// StageRequest 使用同一套操作身份与日志依赖；暂存自己获取锁，不接受外部 mutation 租约。
type StageRequest = SyncRequest

// StageResult 只有已完整校验且不同于当前仓库时才报告 Staged。
type StageResult struct {
	Revision Revision
	Staged   bool
}

type stagingLockSet interface {
	AcquireStaging(context.Context) (mutationLease, error)
}

// Stage 只写独立暂存仓库与 update 事务，允许当前后端保持运行。
func (s *Service) Stage(ctx context.Context, request StageRequest) (result StageResult, returnErr error) {
	if ctx == nil || s == nil || s.newRuntime == nil || s.newLocks == nil {
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, errInvalidServiceRequest)
	}
	if err := validateSyncRequest(request); err != nil {
		return StageResult{}, err
	}
	if request.MutationLease != nil {
		return StageResult{}, serviceInvalidArgumentError(errors.New("staging requires its own mutex lease"))
	}
	if request.Clock == nil {
		request.Clock = time.Now
	}
	if err := requireExistingDirectory(ctx, s.layout); err != nil {
		return StageResult{}, serviceInvalidArgumentError(err)
	}
	logger, err := request.LoggerFactory(ctx, "workspace-stage", request.OperationID)
	if err != nil || nilOperationLogger(logger) {
		if err == nil {
			err = errInvalidServiceRequest
		}
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, err)
	}
	var locks mutationLockSet
	var lease mutationLease
	var runtime syncRuntime
	defer func() { returnErr = joinServiceCloseError(returnErr, runtime, lease, locks, logger, true) }()
	locks, err = s.newLocks(ctx, s.layout)
	if err != nil {
		return StageResult{}, mapMutexFailure(err)
	}
	stager, ok := locks.(stagingLockSet)
	if !ok {
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, errors.New("staging mutex is unavailable"))
	}
	lease, err = stager.AcquireStaging(ctx)
	if err != nil {
		return StageResult{}, mapMutexFailure(err)
	}
	if nilMutationLease(lease) {
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, errInvalidService)
	}
	current, err := s.Check(ctx)
	if err != nil {
		return StageResult{}, err
	}
	if !current.Healthy || current.Version != request.Target.Version() || current.Branch != request.Target.Branch() {
		return StageResult{}, serviceInvalidArgumentError(errors.New("stage target must match the active release branch"))
	}
	runtime, err = s.newRuntime(ctx, s.layout, request, logger)
	if err != nil {
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, err)
	}
	if nilSyncRuntime(runtime) {
		return StageResult{}, serviceInternalError(protocol.StageWorkspaceClone, errInvalidService)
	}
	pending, err := runtime.ReadTransaction(ctx, state.TransactionUpdate)
	if err == nil {
		tx := pending.State()
		if tx.Command != "workspace stage" || (tx.Stage != protocol.StageWorkspaceClone && tx.Stage != protocol.StageWorkspaceVerify) {
			return StageResult{}, newError(protocol.CodeMutationInProgress, protocol.StageWorkspaceClone, messageForCode(protocol.CodeMutationInProgress), nil, errors.New("workspace recovery is required before staging"))
		}
		if tx.Stage == protocol.StageWorkspaceVerify && tx.TargetVersion == current.Version && tx.BaseCommit == current.Commit {
			fetched, err := s.readPreparedFetch(ctx, tx, request.Target)
			if err != nil {
				return StageResult{}, err
			}
			return StageResult{Revision: fetched.Revision, Staged: fetched.Revision.Commit() != current.Commit}, nil
		}
		if _, err := runtime.Recover(ctx, RecoveryRequest{LogPath: logger.LogPath(), DiscardStaged: true}); err != nil {
			return StageResult{}, err
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return StageResult{}, serviceStateWriteError(protocol.StageWorkspaceCheck, err)
	}
	plan, err := s.buildPlan(request.Policy)
	if err != nil {
		return StageResult{}, servicePolicyArgumentError(err)
	}
	tx, err := runtime.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID: request.OperationID, Command: "workspace stage", PID: request.PID,
		TargetVersion: current.Version, BaseCommit: current.Commit, Stage: protocol.StageWorkspaceClone,
	})
	if err != nil {
		return StageResult{}, serviceStateWriteError(protocol.StageWorkspaceClone, err)
	}
	if err := runtime.WriteTransaction(ctx, state.TransactionUpdate, tx); err != nil {
		return StageResult{}, serviceStateWriteError(protocol.StageWorkspaceClone, err)
	}
	fetched, err := runtime.Fetch(ctx, FetchRequest{Plan: plan, Target: request.Target, OperationID: request.OperationID, StageReporter: request.StageReporter})
	if err != nil {
		// 事务必须在删除目录之后移除；清理失败保留完整恢复线索。
		cleanupCtx, cancel := serviceCleanupContext(ctx)
		defer cancel()
		_, cleanupErr := runtime.Recover(cleanupCtx, RecoveryRequest{LogPath: logger.LogPath(), DiscardStaged: true})
		return StageResult{}, errors.Join(err, cleanupErr)
	}
	if fetched.Revision.Commit() == current.Commit {
		_, err := runtime.Recover(ctx, RecoveryRequest{LogPath: logger.LogPath(), DiscardStaged: true})
		return StageResult{Revision: fetched.Revision}, err
	}
	tx.Stage = protocol.StageWorkspaceVerify
	tx.TargetCommit = fetched.Revision.Commit()
	if err := runtime.WriteTransaction(ctx, state.TransactionUpdate, tx); err != nil {
		return StageResult{}, serviceStateWriteError(protocol.StageWorkspaceVerify, err)
	}
	return StageResult{Revision: fetched.Revision, Staged: true}, nil
}

func (s *Service) readPreparedFetch(ctx context.Context, transaction state.TransactionState, target Target) (FetchResult, error) {
	path, err := s.layout.RepoUpdateDir(transaction.OperationID)
	if err != nil {
		return FetchResult{}, serviceInternalError(protocol.StageWorkspaceVerify, err)
	}
	lease, err := filesystem.PinManagedDirectory(ctx, s.layout, path)
	if err != nil || lease == nil {
		if err == nil {
			err = errDirectoryIdentityMissing
		}
		return FetchResult{}, serviceAmbiguousError(protocol.StageWorkspaceVerify, "prepared_update_missing", err)
	}
	identityToken := lease.Identity()
	snapshot, inspectErr := s.reader.Inspect(ctx, path)
	closeErr := lease.Close()
	if inspectErr != nil || closeErr != nil {
		return FetchResult{}, serviceAmbiguousError(protocol.StageWorkspaceVerify, "prepared_update_unreadable", errors.Join(inspectErr, closeErr))
	}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil || identity.version != target.Version() || identity.branch != target.Branch() || identityToken == nil || (transaction.TargetCommit != "" && identity.commit != transaction.TargetCommit) {
		if err == nil {
			err = errors.New("prepared repository identity does not match target")
		}
		return FetchResult{}, serviceAmbiguousError(protocol.StageWorkspaceVerify, "prepared_update_identity", err)
	}
	revision, err := NewRevision(identity.version, identity.branch, identity.commit, identity.sourceKey)
	if err != nil {
		return FetchResult{}, serviceAmbiguousError(protocol.StageWorkspaceVerify, "prepared_update_revision", err)
	}
	return FetchResult{RepositoryPath: path, Revision: revision, DirectoryIdentity: identityToken}, nil
}
