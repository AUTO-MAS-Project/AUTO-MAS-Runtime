package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"

	git "github.com/go-git/go-git/v5"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const (
	recoveryUpdateCleanupReason   = "git-recovery-update"
	recoveryPreviousCleanupReason = "git-recovery-previous"
	recoveryRenameReason          = "git-recovery-rename"
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
	LogPath string
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
	repository, err := r.classifyPath(ctx, paths.repository, false)
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
		return r.recoverBeforeSwap(ctx, transaction, paths, repository, update, previous)
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
	kind     recoveryPathKind
	identity repositoryIdentity
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
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return recoveryPath{kind: recoveryPathMissing}, nil
	case err != nil:
		return recoveryPath{}, fmt.Errorf("inspect recovery path: %w", err)
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return recoveryPath{}, errRecoveryIdentityUnknown
	}
	snapshot, err := r.reader.Inspect(ctx, path)
	if err != nil {
		if allowIncomplete && errors.Is(err, git.ErrRepositoryNotExists) {
			return recoveryPath{kind: recoveryPathIncomplete}, nil
		}
		return recoveryPath{}, fmt.Errorf("inspect recovery repository: %w", err)
	}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil {
		return recoveryPath{}, err
	}
	return recoveryPath{kind: recoveryPathValid, identity: identity}, nil
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
) (RecoveryResult, error) {
	if previous.kind != recoveryPathMissing ||
		(repository.kind != recoveryPathMissing && repository.kind != recoveryPathValid) ||
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
			&result,
		); err != nil {
			return result, err
		}
	}
	return r.finishRecovery(ctx, transaction, result)
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
	targetCount := 0
	for _, candidate := range []recoveryPath{repository, update, previous} {
		if candidate.isTarget(targetVersion) {
			targetCount++
		}
	}
	if targetCount > 1 {
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
				&result,
			); err != nil {
				return result, err
			}
			return r.finishRecovery(ctx, transaction, result)
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
				&result,
			); err != nil {
				return result, err
			}
			if err := r.removeRecoveryTree(
				ctx,
				filesystem.DeleteRepositoryUpdate,
				paths.update,
				transaction.state.OperationID,
				recoveryUpdateCleanupReason,
				&result,
			); err != nil {
				return result, err
			}
			return r.finishRecovery(ctx, transaction, result)

		case update.isTarget(targetVersion) && previous.kind == recoveryPathMissing:
			plan, err := r.prepareEnvironment(ctx, request, update.identity)
			if err != nil {
				return RecoveryResult{}, err
			}
			result := recoveryResultWithIdentity(update.identity, false)
			if err := r.renameRecoveryRepository(
				ctx,
				filesystem.RenameUpdateToRepository,
				paths.update,
				paths.repository,
				transaction.state.OperationID,
				&result,
			); err != nil {
				return result, err
			}
			if err := r.applyEnvironment(ctx, plan, &result); err != nil {
				return result, err
			}
			return r.finishRecovery(ctx, transaction, result)

		case update.kind == recoveryPathMissing && previous.kind == recoveryPathValid:
			result := RecoveryResult{}
			if err := r.renameRecoveryRepository(
				ctx,
				filesystem.RenameRepositoryRollback,
				paths.previous,
				paths.repository,
				transaction.state.OperationID,
				&result,
			); err != nil {
				return result, err
			}
			return r.finishRecovery(ctx, transaction, result)
		}
	}

	return RecoveryResult{}, recoveryAmbiguousError(
		transaction.state.Stage,
		"swap_shape",
		errRecoveryIdentityUnknown,
	)
}

func (r *Recovery) completeActiveTarget(
	ctx context.Context,
	request RecoveryRequest,
	transaction recoveryTransaction,
	paths recoveryPaths,
	identity repositoryIdentity,
	previous recoveryPath,
) (RecoveryResult, error) {
	plan, err := r.prepareEnvironment(ctx, request, identity)
	if err != nil {
		return RecoveryResult{}, err
	}
	result := recoveryResultWithIdentity(identity, true)
	if previous.exists() {
		if err := r.removeRecoveryTree(
			ctx,
			filesystem.DeleteRepositoryRetired,
			paths.previous,
			transaction.state.OperationID,
			recoveryPreviousCleanupReason,
			&result,
		); err != nil {
			return result, err
		}
	}
	if err := r.applyEnvironment(ctx, plan, &result); err != nil {
		return result, err
	}
	return r.finishRecovery(ctx, transaction, result)
}

type environmentRecoveryPlan struct {
	write bool
	value state.EnvironmentState
}

func (r *Recovery) prepareEnvironment(
	ctx context.Context,
	request RecoveryRequest,
	identity repositoryIdentity,
) (environmentRecoveryPlan, error) {
	environment, err := r.store.ReadEnvironment(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		if isCancellation(ctx, err) {
			return environmentRecoveryPlan{}, recoveryCancelledError(protocol.StageWorkspaceSwap, err)
		}
		return environmentRecoveryPlan{}, recoveryAmbiguousError(
			protocol.StageWorkspaceSwap,
			"environment_unreadable",
			err,
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
) error {
	if !plan.write {
		return nil
	}
	if err := r.store.WriteEnvironment(ctx, plan.value); err != nil {
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
	result *RecoveryResult,
) error {
	renameResult, err := r.operator.AtomicRename(ctx, filesystem.RenameRequest{
		Kind:        kind,
		Source:      source,
		Destination: destination,
		OperationID: operationID,
		Reason:      recoveryRenameReason,
	})
	result.MutationApplied = result.MutationApplied || renameResult.MutationApplied
	if err != nil {
		return mapRenameFailure(ctx, err)
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
	result *RecoveryResult,
) error {
	deleteResult, err := r.operator.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:        kind,
		Target:      target,
		OperationID: operationID,
		Reason:      reason,
	})
	result.MutationApplied = result.MutationApplied || deleteResult.Removed || deleteResult.Partial
	if err != nil {
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
) (RecoveryResult, error) {
	if err := transaction.remove(ctx); err != nil {
		return result, mapTransactionWriteFailure(ctx, transaction.state.Stage, err)
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
	return newError(
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
