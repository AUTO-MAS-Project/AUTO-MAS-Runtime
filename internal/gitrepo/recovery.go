package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const (
	recoveryUpdateCleanupReason   = "git-recovery-update"
	recoveryPreviousCleanupReason = "git-recovery-previous"
	recoveryRenameReason          = "git-recovery-rename"
	recoveryCleanupTimeout        = 30 * time.Second
)

var (
	errInvalidRecovery           = errors.New("repository recovery is invalid")
	errInvalidRecoveryRequest    = errors.New("repository recovery request is invalid")
	errRecoveryIdentityUnknown   = errors.New("repository recovery identity is unknown")
	errRecoveryCleanupIncomplete = errors.New("repository recovery cleanup is incomplete")
)

type recoveryTransaction struct {
	state  state.TransactionState
	remove func(ctx context.Context) error
}

type recoveryStateStore interface {
	ReadUpdate(ctx context.Context) (recoveryTransaction, error)
	ReadEnvironment(ctx context.Context) (state.EnvironmentState, error)
	NewBrokenEnvironment(
		lastSuccessful state.Revision,
		broken state.BrokenEnvironment,
	) (state.EnvironmentState, error)
	WriteEnvironment(ctx context.Context, value state.EnvironmentState) error
}

type stateRecoveryStore struct {
	store *state.Store
}

func (s *stateRecoveryStore) ReadUpdate(ctx context.Context) (recoveryTransaction, error) {
	snapshot, err := s.store.ReadTransaction(ctx, state.TransactionUpdate)
	if err != nil {
		return recoveryTransaction{}, err
	}
	return recoveryTransaction{
		state: snapshot.State(),
		remove: func(removeCtx context.Context) error {
			return s.store.RemoveTransaction(removeCtx, snapshot)
		},
	}, nil
}

func (s *stateRecoveryStore) ReadEnvironment(ctx context.Context) (state.EnvironmentState, error) {
	return s.store.ReadEnvironment(ctx)
}

func (s *stateRecoveryStore) NewBrokenEnvironment(
	lastSuccessful state.Revision,
	broken state.BrokenEnvironment,
) (state.EnvironmentState, error) {
	return s.store.NewBrokenEnvironment(lastSuccessful, broken)
}

func (s *stateRecoveryStore) WriteEnvironment(
	ctx context.Context,
	value state.EnvironmentState,
) error {
	return s.store.WriteEnvironment(ctx, value)
}

// RecoveryRequest 提供补写 repository_changed 所需的当前 Runtime 日志路径。
type RecoveryRequest struct {
	LogPath       string
	StageReporter StageReporter
}

// RecoveryResult 保存恢复动作和已证明 active revision 的事实。
type RecoveryResult struct {
	Recovered          bool
	MutationApplied    bool
	EnvironmentWritten bool
	TransactionRemoved bool
	Version            string
	Branch             string
	Commit             string
	SourceKey          string
}

// Recovery 按 update transaction 和三个精确目录身份恢复中断现场。
type Recovery struct {
	layout   *config.Layout
	operator repositorySwapOperator
	store    recoveryStateStore
	reader   repositoryReader
}

// NewRecovery 创建使用真实 filesystem、state Store 和 go-git reader 的恢复服务。
func NewRecovery(
	layout *config.Layout,
	operator *filesystem.Operator,
	store *state.Store,
) (*Recovery, error) {
	if store == nil {
		return nil, errInvalidRecovery
	}
	return newRecoveryWithDependencies(
		layout,
		operator,
		&stateRecoveryStore{store: store},
		goGitRepositoryReader{},
	)
}

func newRecoveryWithDependencies(
	layout *config.Layout,
	operator repositorySwapOperator,
	store recoveryStateStore,
	reader repositoryReader,
) (*Recovery, error) {
	if layout == nil || nilDependency(operator) || nilDependency(store) || nilDependency(reader) {
		return nil, errInvalidRecovery
	}
	return &Recovery{
		layout:   layout,
		operator: operator,
		store:    store,
		reader:   reader,
	}, nil
}

// Recover 在任何目录副作用前完成全部必要身份分类。
func (r *Recovery) Recover(
	ctx context.Context,
	request RecoveryRequest,
) (RecoveryResult, error) {
	if ctx == nil || r == nil || r.layout == nil ||
		nilDependency(r.operator) || nilDependency(r.store) || nilDependency(r.reader) {
		return RecoveryResult{}, recoveryInternalError(errInvalidRecovery)
	}
	reportRecoveryStage(request, protocol.StageWorkspaceCleanup)
	transaction, err := r.store.ReadUpdate(ctx)
	if errors.Is(err, state.ErrNotFound) {
		return RecoveryResult{}, nil
	}
	if err != nil {
		if isCancellation(ctx, err) {
			return RecoveryResult{}, recoveryCancelledError(protocol.StageWorkspaceCleanup, err)
		}
		return RecoveryResult{}, recoveryAmbiguousError(
			protocol.StageWorkspaceCleanup,
			"transaction_unreadable",
			err,
		)
	}
	if transaction.remove == nil ||
		state.ValidateTransaction(state.TransactionUpdate, transaction.state) != nil {
		return RecoveryResult{}, recoveryAmbiguousError(
			protocol.StageWorkspaceCleanup,
			"transaction_invalid",
			errInvalidRecoveryRequest,
		)
	}

	paths, err := r.recoveryPaths(transaction.state.OperationID)
	if err != nil {
		return RecoveryResult{}, recoveryInternalError(err)
	}
	allowIncompleteUpdate := transaction.state.Stage == protocol.StageWorkspaceClone
	allowDamagedRepository := transaction.state.Stage == protocol.StageWorkspaceClone ||
		transaction.state.Stage == protocol.StageWorkspaceVerify
	repository, err := r.classifyPath(ctx, paths.repository, allowDamagedRepository)
	if err != nil {
		return RecoveryResult{}, r.classificationError(ctx, transaction.state.Stage, "repository_unknown", err)
	}
	update, err := r.classifyPath(ctx, paths.update, allowIncompleteUpdate)
	if err != nil {
		return RecoveryResult{}, r.classificationError(ctx, transaction.state.Stage, "update_unknown", err)
	}
	previous, err := r.classifyPath(ctx, paths.previous, false)
	if err != nil {
		return RecoveryResult{}, r.classificationError(ctx, transaction.state.Stage, "previous_unknown", err)
	}

	switch transaction.state.Stage {
	case protocol.StageWorkspaceClone, protocol.StageWorkspaceVerify:
		return r.recoverBeforeSwap(ctx, transaction, paths, repository, update, previous, request.StageReporter)
	case protocol.StageWorkspaceSwap, protocol.StageWorkspaceCleanup:
		return r.recoverSwap(ctx, request, transaction, paths, repository, update, previous)
	default:
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"stage_invalid",
			errInvalidRecoveryRequest,
		)
	}
}

type recoveryPaths struct {
	repository string
	update     string
	previous   string
}

func (r *Recovery) recoveryPaths(operationID string) (recoveryPaths, error) {
	update, updateErr := r.layout.RepoUpdateDir(operationID)
	previous, previousErr := r.layout.RepoPreviousDir(operationID)
	if err := errors.Join(updateErr, previousErr); err != nil {
		return recoveryPaths{}, fmt.Errorf("derive recovery paths: %w", err)
	}
	return recoveryPaths{
		repository: r.layout.RepoDir(),
		update:     update,
		previous:   previous,
	}, nil
}

type recoveryPathKind uint8

const (
	recoveryPathMissing recoveryPathKind = iota
	recoveryPathIncomplete
	recoveryPathValid
)

type recoveryPath struct {
	kind              recoveryPathKind
	identity          repositoryIdentity
	directoryIdentity *filesystem.DirectoryIdentity
}

func (p recoveryPath) exists() bool {
	return p.kind != recoveryPathMissing
}

func (p recoveryPath) isTarget(version string) bool {
	return p.kind == recoveryPathValid && p.identity.version == version
}

func (r *Recovery) classifyPath(
	ctx context.Context,
	path string,
	allowIncomplete bool,
) (recoveryPath, error) {
	if err := ctx.Err(); err != nil {
		return recoveryPath{}, err
	}
	inspection, err := filesystem.InspectManagedDirectory(ctx, r.layout, path)
	if err != nil {
		return recoveryPath{}, fmt.Errorf("inspect recovery path: %w", err)
	}
	if !inspection.Exists {
		return recoveryPath{kind: recoveryPathMissing}, nil
	}
	if inspection.Identity == nil {
		return recoveryPath{}, errRecoveryIdentityUnknown
	}
	lease, err := filesystem.PinManagedDirectory(ctx, r.layout, path)
	if err != nil {
		return recoveryPath{}, fmt.Errorf("pin recovery path: %w", err)
	}
	if lease == nil {
		return recoveryPath{}, errRecoveryIdentityUnknown
	}
	directoryIdentity := lease.Identity()
	if directoryIdentity == nil {
		closeErr := lease.Close()
		return recoveryPath{}, errors.Join(errRecoveryIdentityUnknown, closeErr)
	}
	if !inspection.Identity.Equal(directoryIdentity) {
		closeErr := lease.Close()
		return recoveryPath{}, errors.Join(filesystem.ErrIdentityChanged, closeErr)
	}
	snapshot, inspectErr := r.reader.Inspect(ctx, path)
	closeErr := lease.Close()
	if closeErr != nil {
		inspectErr = errors.Join(inspectErr, closeErr)
	}
	err = inspectErr
	if closeErr != nil {
		return recoveryPath{}, fmt.Errorf("close recovery path lease: %w", err)
	}
	if err != nil {
		if allowIncomplete && !isCancellation(ctx, err) && !errors.Is(err, os.ErrPermission) {
			return recoveryPath{kind: recoveryPathIncomplete, directoryIdentity: directoryIdentity}, nil
		}
		return recoveryPath{}, fmt.Errorf("inspect recovery repository: %w", err)
	}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil {
		return recoveryPath{}, err
	}
	return recoveryPath{
		kind:              recoveryPathValid,
		identity:          identity,
		directoryIdentity: directoryIdentity,
	}, nil
}

func (r *Recovery) classificationError(
	ctx context.Context,
	stage protocol.Stage,
	reason string,
	cause error,
) *Error {
	if isCancellation(ctx, cause) {
		return recoveryCancelledError(stage, cause)
	}
	return recoveryAmbiguousError(stage, reason, cause)
}

func (r *Recovery) recoverBeforeSwap(
	ctx context.Context,
	transaction recoveryTransaction,
	paths recoveryPaths,
	repository recoveryPath,
	update recoveryPath,
	previous recoveryPath,
	reporter StageReporter,
) (RecoveryResult, error) {
	// 首次安装尚未创建 update 时可只清理事务；update 已能证明目标 revision 时，
	// 缺失 repo 意味着唯一有效候选无法恢复，必须保留现场。
	if repository.kind == recoveryPathMissing && update.isTarget(transaction.state.TargetVersion) {
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"pre_swap_repository_missing",
			errRecoveryIdentityUnknown,
		)
	}
	if repository.isTarget(transaction.state.TargetVersion) &&
		update.isTarget(transaction.state.TargetVersion) &&
		!repository.identity.sameRevision(update.identity) {
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"multiple_target_commits",
			errRecoveryIdentityUnknown,
		)
	}
	if previous.kind != recoveryPathMissing ||
		(repository.kind != recoveryPathMissing && repository.kind != recoveryPathIncomplete && repository.kind != recoveryPathValid) ||
		(update.kind == recoveryPathValid && !update.isTarget(transaction.state.TargetVersion)) ||
		(update.kind == recoveryPathIncomplete && transaction.state.Stage != protocol.StageWorkspaceClone) {
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"pre_swap_identity",
			errRecoveryIdentityUnknown,
		)
	}
	result := RecoveryResult{}
	if update.exists() {
		if err := r.removeRecoveryTree(
			ctx,
			filesystem.DeleteRepositoryUpdate,
			paths.update,
			transaction.state.OperationID,
			recoveryUpdateCleanupReason,
			update.directoryIdentity,
			&result,
			reporter,
			false,
		); err != nil {
			return result, err
		}
	}
	return r.finishRecovery(ctx, transaction, result, reporter, false)
}

func (r *Recovery) recoverSwap(
	ctx context.Context,
	request RecoveryRequest,
	transaction recoveryTransaction,
	paths recoveryPaths,
	repository recoveryPath,
	update recoveryPath,
	previous recoveryPath,
) (RecoveryResult, error) {
	if repository.kind == recoveryPathIncomplete ||
		update.kind == recoveryPathIncomplete ||
		previous.kind == recoveryPathIncomplete {
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"swap_incomplete",
			errRecoveryIdentityUnknown,
		)
	}
	targetVersion := transaction.state.TargetVersion
	if hasMultipleTargetRevisions(targetVersion, repository, update, previous) {
		return RecoveryResult{}, recoveryAmbiguousError(
			transaction.state.Stage,
			"multiple_target_candidates",
			errRecoveryIdentityUnknown,
		)
	}

	switch {
	case repository.isTarget(targetVersion):
		if update.kind != recoveryPathMissing ||
			previous.kind != recoveryPathMissing && previous.kind != recoveryPathValid {
			return RecoveryResult{}, recoveryAmbiguousError(
				transaction.state.Stage,
				"active_target_shape",
				errRecoveryIdentityUnknown,
			)
		}
		return r.completeActiveTarget(ctx, request, transaction, paths, repository.identity, previous)

	case repository.kind == recoveryPathValid:
		if update.isTarget(targetVersion) && previous.kind == recoveryPathMissing {
			result := RecoveryResult{}
			if err := r.removeRecoveryTree(
				ctx,
				filesystem.DeleteRepositoryUpdate,
				paths.update,
				transaction.state.OperationID,
				recoveryUpdateCleanupReason,
				update.directoryIdentity,
				&result,
				request.StageReporter,
				false,
			); err != nil {
				return result, err
			}
			return r.finishRecovery(ctx, transaction, result, request.StageReporter, false)
		}

	case repository.kind == recoveryPathMissing:
		switch {
		case update.isTarget(targetVersion) && previous.kind == recoveryPathValid:
			result := RecoveryResult{}
			if err := r.renameRecoveryRepository(
				ctx,
				filesystem.RenameRepositoryRollback,
				paths.previous,
				paths.repository,
				transaction.state.OperationID,
				previous.directoryIdentity,
				&result,
				request.StageReporter,
			); err != nil {
				return result, err
			}
			cleanupCtx, cancelCleanup := recoveryCommittedContext(ctx)
			defer cancelCleanup()
			if err := r.removeRecoveryTree(
				cleanupCtx,
				filesystem.DeleteRepositoryUpdate,
				paths.update,
				transaction.state.OperationID,
				recoveryUpdateCleanupReason,
				update.directoryIdentity,
				&result,
				request.StageReporter,
				true,
			); err != nil {
				return result, err
			}
			return r.finishRecovery(cleanupCtx, transaction, result, request.StageReporter, true)

		case update.isTarget(targetVersion) && previous.kind == recoveryPathMissing:
			plan, err := r.prepareEnvironment(ctx, request, update.identity, false)
			if err != nil {
				return RecoveryResult{}, err
			}
			result := recoveryResultWithIdentity(update.identity, false)
			renameErr := r.renameRecoveryRepository(
				ctx,
				filesystem.RenameUpdateToRepository,
				paths.update,
				paths.repository,
				transaction.state.OperationID,
				update.directoryIdentity,
				&result,
				request.StageReporter,
			)
			if renameErr != nil {
				if !result.MutationApplied {
					return result, renameErr
				}
				// rename 已激活目标但收尾报告错误时，先脱离业务取消持久化
				// repository_changed；保留 transaction 让下一次 Recovery 收口现场。
				cleanupCtx, cancelCleanup := recoveryCommittedContext(ctx)
				defer cancelCleanup()
				if environmentErr := r.applyEnvironment(cleanupCtx, plan, &result, request.StageReporter, true); environmentErr != nil {
					return result, errors.Join(renameErr, environmentErr)
				}
				return result, renameErr
			}
			cleanupCtx, cancelCleanup := recoveryCommittedContext(ctx)
			defer cancelCleanup()
			if err := r.applyEnvironment(cleanupCtx, plan, &result, request.StageReporter, true); err != nil {
				return result, err
			}
			return r.finishRecovery(cleanupCtx, transaction, result, request.StageReporter, true)

		case update.kind == recoveryPathMissing && previous.kind == recoveryPathValid:
			result := RecoveryResult{}
			if err := r.renameRecoveryRepository(
				ctx,
				filesystem.RenameRepositoryRollback,
				paths.previous,
				paths.repository,
				transaction.state.OperationID,
				previous.directoryIdentity,
				&result,
				request.StageReporter,
			); err != nil {
				return result, err
			}
			cleanupCtx, cancelCleanup := recoveryCommittedContext(ctx)
			defer cancelCleanup()
			return r.finishRecovery(cleanupCtx, transaction, result, request.StageReporter, true)
		}
	}

	return RecoveryResult{}, recoveryAmbiguousError(
		transaction.state.Stage,
		"swap_shape",
		errRecoveryIdentityUnknown,
	)
}

func hasMultipleTargetRevisions(targetVersion string, candidates ...recoveryPath) bool {
	var first repositoryIdentity
	found := false
	for _, candidate := range candidates {
		if !candidate.isTarget(targetVersion) {
			continue
		}
		if !found {
			first = candidate.identity
			found = true
			continue
		}
		if !first.sameRevision(candidate.identity) {
			return true
		}
	}
	return false
}

func (r *Recovery) completeActiveTarget(
	ctx context.Context,
	request RecoveryRequest,
	transaction recoveryTransaction,
	paths recoveryPaths,
	identity repositoryIdentity,
	previous recoveryPath,
) (RecoveryResult, error) {
	cleanupCtx, cancelCleanup := recoveryCommittedContext(ctx)
	defer cancelCleanup()
	plan, err := r.prepareEnvironment(cleanupCtx, request, identity, true)
	if err != nil {
		return RecoveryResult{}, err
	}
	result := recoveryResultWithIdentity(identity, true)
	// active repo 已跨过 swap 提交点；先持久化 repository_changed，避免
	// retired cleanup 失败时稳定状态仍宣称旧仓库可用。
	if err := r.applyEnvironment(cleanupCtx, plan, &result, request.StageReporter, true); err != nil {
		return result, err
	}
	if previous.exists() {
		if err := r.removeRecoveryTree(
			cleanupCtx,
			filesystem.DeleteRepositoryRetired,
			paths.previous,
			transaction.state.OperationID,
			recoveryPreviousCleanupReason,
			previous.directoryIdentity,
			&result,
			request.StageReporter,
			true,
		); err != nil {
			return result, err
		}
	}
	return r.finishRecovery(cleanupCtx, transaction, result, request.StageReporter, true)
}

func recoveryCommittedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recoveryCleanupTimeout)
}

type environmentRecoveryPlan struct {
	write bool
	value state.EnvironmentState
}

func (r *Recovery) prepareEnvironment(
	ctx context.Context,
	request RecoveryRequest,
	identity repositoryIdentity,
	committed bool,
) (environmentRecoveryPlan, error) {
	// 环境补写属于 swap 提交后的状态收口，必须与目录 rename 使用同一 stage。
	reportRecoveryStage(request, protocol.StageWorkspaceSwap)
	environment, err := r.store.ReadEnvironment(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		if isCancellation(ctx, err) {
			if committed {
				return environmentRecoveryPlan{}, newCommittedError(
					protocol.CodeStateWriteFailed,
					protocol.StageWorkspaceSwap,
					messageForCode(protocol.CodeStateWriteFailed),
					map[string]any{},
					err,
				)
			}
			return environmentRecoveryPlan{}, recoveryCancelledError(protocol.StageWorkspaceSwap, err)
		}
		return environmentRecoveryPlan{}, recoveryAmbiguousErrorWithCommitted(
			protocol.StageWorkspaceSwap,
			"environment_unreadable",
			err,
			committed,
		)
	}
	lastSuccessful := state.Revision{}
	if err == nil {
		lastSuccessful = environment.LastSuccessful
		if environment.Status == protocol.StateEnvironmentBroken &&
			environment.Broken != nil &&
			environment.Broken.Reason == state.ReasonRepositoryChanged &&
			environment.Broken.TargetVersion == identity.version &&
			environment.Broken.Branch == identity.branch &&
			environment.Broken.Commit == identity.commit {
			return environmentRecoveryPlan{}, nil
		}
	}
	if request.LogPath == "" {
		if committed {
			return environmentRecoveryPlan{}, newCommittedError(
				protocol.CodeInternalError,
				protocol.StageWorkspaceSwap,
				messageForCode(protocol.CodeInternalError),
				map[string]any{},
				errInvalidRecoveryRequest,
			)
		}
		return environmentRecoveryPlan{}, recoveryInternalError(errInvalidRecoveryRequest)
	}
	value, err := r.store.NewBrokenEnvironment(lastSuccessful, state.BrokenEnvironment{
		TargetVersion: identity.version,
		Branch:        identity.branch,
		Commit:        identity.commit,
		PythonVersion: "",
		UVVersion:     "",
		Reason:        state.ReasonRepositoryChanged,
		Stage:         protocol.StageWorkspaceSwap,
		ExitCode:      0,
		LogPath:       request.LogPath,
	})
	if err != nil {
		if committed {
			return environmentRecoveryPlan{}, newCommittedError(
				protocol.CodeStateWriteFailed,
				protocol.StageWorkspaceSwap,
				messageForCode(protocol.CodeStateWriteFailed),
				map[string]any{},
				err,
			)
		}
		return environmentRecoveryPlan{}, newError(
			protocol.CodeStateWriteFailed,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeStateWriteFailed),
			map[string]any{},
			err,
		)
	}
	return environmentRecoveryPlan{write: true, value: value}, nil
}

func (r *Recovery) applyEnvironment(
	ctx context.Context,
	plan environmentRecoveryPlan,
	result *RecoveryResult,
	reporter StageReporter,
	committed bool,
) error {
	if !plan.write {
		return nil
	}
	reportRecoveryStageValue(reporter, protocol.StageWorkspaceSwap)
	if err := r.store.WriteEnvironment(ctx, plan.value); err != nil {
		if committed {
			return mapCommittedTransactionWriteFailure(protocol.StageWorkspaceSwap, err)
		}
		return mapTransactionWriteFailure(ctx, protocol.StageWorkspaceSwap, err)
	}
	result.EnvironmentWritten = true
	return nil
}

func (r *Recovery) renameRecoveryRepository(
	ctx context.Context,
	kind filesystem.RenameKind,
	source string,
	destination string,
	operationID string,
	expectedSourceIdentity *filesystem.DirectoryIdentity,
	result *RecoveryResult,
	reporter StageReporter,
) error {
	// rename 是恢复阶段唯一的目录激活动作，先更新 stage 再调用受控句柄能力。
	reportRecoveryStageValue(reporter, protocol.StageWorkspaceSwap)
	renameResult, err := r.operator.AtomicRename(ctx, filesystem.RenameRequest{
		Kind:                   kind,
		Source:                 source,
		Destination:            destination,
		OperationID:            operationID,
		Reason:                 recoveryRenameReason,
		ExpectedSourceIdentity: expectedSourceIdentity,
	})
	result.MutationApplied = result.MutationApplied || renameResult.MutationApplied
	if err != nil {
		if isAmbiguousFilesystemError(err) && !renameResult.MutationApplied {
			return recoveryAmbiguousError(protocol.StageWorkspaceSwap, "directory_identity_changed", err)
		}
		return mapRenameFailure(
			ctx,
			err,
			renameResult.MutationApplied,
			kind == filesystem.RenameUpdateToRepository && renameResult.MutationApplied,
		)
	}
	if !renameResult.MutationApplied {
		return swapFailureError(protocol.StageWorkspaceSwap, errSwapNotApplied)
	}
	return nil
}

func (r *Recovery) removeRecoveryTree(
	ctx context.Context,
	kind filesystem.DeleteKind,
	target string,
	operationID string,
	reason string,
	expectedIdentity *filesystem.DirectoryIdentity,
	result *RecoveryResult,
	reporter StageReporter,
	committed bool,
) error {
	// 删除 update/previous 属于 cleanup；Recovery 的调用方可据此正确关联 warning。
	reportRecoveryStageValue(reporter, protocol.StageWorkspaceCleanup)
	deleteResult, err := r.operator.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:             kind,
		Target:           target,
		OperationID:      operationID,
		Reason:           reason,
		ExpectedIdentity: expectedIdentity,
	})
	result.MutationApplied = result.MutationApplied || deleteResult.Removed || deleteResult.Partial
	if err != nil {
		if isAmbiguousFilesystemError(err) {
			if committed {
				return mapCommittedCleanupFailure(deleteResult, err)
			}
			return recoveryAmbiguousError(protocol.StageWorkspaceCleanup, "directory_identity_changed", err)
		}
		if committed {
			return mapCommittedCleanupFailure(deleteResult, err)
		}
		if isCancellation(ctx, err) {
			return recoveryCancelledError(protocol.StageWorkspaceCleanup, err)
		}
		return newError(
			protocol.CodeGitRepoCleanupFailed,
			protocol.StageWorkspaceCleanup,
			messageForCode(protocol.CodeGitRepoCleanupFailed),
			map[string]any{},
			err,
		)
	}
	if deleteResult.Partial || !deleteResult.AuditCompleted {
		if committed {
			return mapCommittedCleanupFailure(deleteResult, errRecoveryCleanupIncomplete)
		}
		return newError(
			protocol.CodeGitRepoCleanupFailed,
			protocol.StageWorkspaceCleanup,
			messageForCode(protocol.CodeGitRepoCleanupFailed),
			map[string]any{},
			errRecoveryCleanupIncomplete,
		)
	}
	return nil
}

func (r *Recovery) finishRecovery(
	ctx context.Context,
	transaction recoveryTransaction,
	result RecoveryResult,
	reporter StageReporter,
	committed bool,
) (RecoveryResult, error) {
	reportRecoveryStageValue(reporter, protocol.StageWorkspaceCleanup)
	if err := transaction.remove(ctx); err != nil {
		if committed {
			return result, mapCommittedTransactionWriteFailure(protocol.StageWorkspaceCleanup, err)
		}
		return result, mapTransactionWriteFailure(ctx, protocol.StageWorkspaceCleanup, err)
	}
	result.Recovered = true
	result.TransactionRemoved = true
	return result, nil
}

func recoveryResultWithIdentity(identity repositoryIdentity, mutationApplied bool) RecoveryResult {
	return RecoveryResult{
		MutationApplied: mutationApplied,
		Version:         identity.version,
		Branch:          identity.branch,
		Commit:          identity.commit,
		SourceKey:       identity.sourceKey,
	}
}

func recoveryAmbiguousError(
	stage protocol.Stage,
	reason string,
	cause error,
) *Error {
	return recoveryAmbiguousErrorWithCommitted(stage, reason, cause, false)
}

func recoveryAmbiguousErrorWithCommitted(
	stage protocol.Stage,
	reason string,
	cause error,
	committed bool,
) *Error {
	constructor := newError
	if committed {
		constructor = newCommittedError
	}
	return constructor(
		protocol.CodeUpdateStateAmbiguous,
		stage,
		messageForCode(protocol.CodeUpdateStateAmbiguous),
		map[string]any{"reason": reason},
		cause,
	)
}

func recoveryCancelledError(stage protocol.Stage, cause error) *Error {
	return newError(
		protocol.CodeOperationCancelled,
		stage,
		messageForCode(protocol.CodeOperationCancelled),
		map[string]any{},
		cause,
	)
}

func recoveryInternalError(cause error) *Error {
	return newError(
		protocol.CodeInternalError,
		protocol.StageWorkspaceCleanup,
		messageForCode(protocol.CodeInternalError),
		map[string]any{},
		cause,
	)
}

var (
	_ recoveryStateStore = (*stateRecoveryStore)(nil)
	_ repositoryReader   = goGitRepositoryReader{}
)

func reportRecoveryStage(request RecoveryRequest, stage protocol.Stage) {
	reportRecoveryStageValue(request.StageReporter, stage)
}

func reportRecoveryStageValue(reporter StageReporter, stage protocol.Stage) {
	if reporter != nil {
		reporter(stage)
	}
}
