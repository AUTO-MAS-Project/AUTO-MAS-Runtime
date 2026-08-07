package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const (
	swapRenameReason   = "git-repository-swap"
	swapCleanupReason  = "git-repository-retired"
	swapCleanupTimeout = 30 * time.Second
)

var (
	errInvalidSwapper     = errors.New("repository swapper is invalid")
	errInvalidSwapRequest = errors.New("repository swap request is invalid")
	errSwapNotApplied     = errors.New("repository swap mutation was not applied")
)

type repositorySwapOperator interface {
	AtomicRename(
		ctx context.Context,
		request filesystem.RenameRequest,
	) (filesystem.RenameResult, error)
	RemoveTree(
		ctx context.Context,
		request filesystem.DeleteRequest,
	) (filesystem.DeleteResult, error)
}

type updateTransactionWriter interface {
	WriteTransaction(
		ctx context.Context,
		kind state.TransactionKind,
		value state.TransactionState,
	) error
}

// SwapRequest 把已验证 Revision 绑定到当前 update transaction。
type SwapRequest struct {
	Transaction    state.TransactionState
	Revision       Revision
	ActiveIdentity *filesystem.DirectoryIdentity
	UpdateIdentity *filesystem.DirectoryIdentity
	StageReporter  StageReporter
	// CommitEnvironment 在新仓库激活且 cleanup transaction 持久化后调用。
	// 回调必须先持久化 repository_changed，Swapper 才会删除 previous。
	CommitEnvironment func(context.Context, Revision) error
}

// SwapResult 保存目录副作用、激活提交点和旧目录收口事实。
type SwapResult struct {
	Revision            Revision
	MutationApplied     bool
	RepositoryActivated bool
	CleanupCompleted    bool
}

// Swapper 通过封闭 filesystem 能力整体替换受管仓库。
type Swapper struct {
	layout       *config.Layout
	operator     repositorySwapOperator
	transactions updateTransactionWriter
}

// NewSwapper 创建使用真实受控文件系统和状态 Store 的仓库替换器。
func NewSwapper(
	layout *config.Layout,
	operator *filesystem.Operator,
	transactions *state.Store,
) (*Swapper, error) {
	return newSwapperWithDependencies(layout, operator, transactions)
}

func newSwapperWithDependencies(
	layout *config.Layout,
	operator repositorySwapOperator,
	transactions updateTransactionWriter,
) (*Swapper, error) {
	if layout == nil || nilDependency(operator) || nilDependency(transactions) {
		return nil, errInvalidSwapper
	}
	return &Swapper{
		layout:       layout,
		operator:     operator,
		transactions: transactions,
	}, nil
}

// Swap 在 update transaction 保护下执行两次 rename 和旧目录收口。
func (s *Swapper) Swap(ctx context.Context, request SwapRequest) (SwapResult, error) {
	if ctx == nil || s == nil || s.layout == nil ||
		nilDependency(s.operator) || nilDependency(s.transactions) {
		return SwapResult{}, swapInternalError(errInvalidSwapper)
	}
	if err := validateSwapRequest(request); err != nil {
		return SwapResult{}, swapInternalError(err)
	}
	reportSwapStage(request, protocol.StageWorkspaceSwap)
	if err := ctx.Err(); err != nil {
		return SwapResult{}, swapCancelledError(err)
	}
	if request.UpdateIdentity == nil {
		return SwapResult{}, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "directory_identity_unavailable"},
			errDirectoryIdentityMissing,
		)
	}
	previousPath, err := s.layout.RepoPreviousDir(request.Transaction.OperationID)
	if err != nil {
		return SwapResult{}, swapInternalError(fmt.Errorf("derive previous repository path: %w", err))
	}
	updatePath, err := s.layout.RepoUpdateDir(request.Transaction.OperationID)
	if err != nil {
		return SwapResult{}, swapInternalError(fmt.Errorf("derive update repository path: %w", err))
	}
	result := SwapResult{Revision: request.Revision}

	swapTransaction := request.Transaction
	swapTransaction.Stage = protocol.StageWorkspaceSwap
	if err := s.transactions.WriteTransaction(ctx, state.TransactionUpdate, swapTransaction); err != nil {
		return result, mapTransactionWriteFailure(ctx, protocol.StageWorkspaceSwap, err)
	}

	previousExists, err := pathExists(ctx, s.layout, previousPath)
	if err != nil {
		if isCancellation(ctx, err) {
			return result, swapCancelledError(err)
		}
		return result, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "previous_unreadable"},
			err,
		)
	}
	if previousExists {
		return result, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "previous_exists"},
			errors.New("previous repository path already exists"),
		)
	}

	repositoryInspection, err := filesystem.InspectManagedDirectory(ctx, s.layout, s.layout.RepoDir())
	if err != nil {
		if isCancellation(ctx, err) {
			return result, swapCancelledError(err)
		}
		if isAmbiguousFilesystemError(err) {
			return result, newError(
				protocol.CodeUpdateStateAmbiguous,
				protocol.StageWorkspaceSwap,
				messageForCode(protocol.CodeUpdateStateAmbiguous),
				map[string]any{"reason": "repository_identity_unavailable"},
				err,
			)
		}
		return result, swapFailureError(protocol.StageWorkspaceSwap, err)
	}
	repositoryExists := repositoryInspection.Exists
	if repositoryExists && repositoryInspection.Identity == nil {
		return result, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "repository_identity_unavailable"},
			errDirectoryIdentityMissing,
		)
	}
	if repositoryExists && request.ActiveIdentity == nil {
		return result, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "repository_identity_unavailable"},
			errDirectoryIdentityMissing,
		)
	}
	if repositoryExists {
		renameResult, renameErr := s.operator.AtomicRename(ctx, filesystem.RenameRequest{
			Kind:                   filesystem.RenameRepositoryToRetired,
			Source:                 s.layout.RepoDir(),
			Destination:            previousPath,
			OperationID:            request.Transaction.OperationID,
			Reason:                 swapRenameReason,
			ExpectedSourceIdentity: request.ActiveIdentity,
		})
		result.MutationApplied = renameResult.MutationApplied
		if renameErr != nil {
			return result, mapRenameFailure(ctx, renameErr, renameResult.MutationApplied, false)
		}
		if !renameResult.MutationApplied {
			return result, swapFailureError(protocol.StageWorkspaceSwap, errSwapNotApplied)
		}
	}

	renameResult, renameErr := s.operator.AtomicRename(ctx, filesystem.RenameRequest{
		Kind:                   filesystem.RenameUpdateToRepository,
		Source:                 updatePath,
		Destination:            s.layout.RepoDir(),
		OperationID:            request.Transaction.OperationID,
		Reason:                 swapRenameReason,
		ExpectedSourceIdentity: request.UpdateIdentity,
	})
	result.MutationApplied = result.MutationApplied || renameResult.MutationApplied
	result.RepositoryActivated = renameResult.MutationApplied
	if renameErr != nil {
		return result, mapRenameFailure(ctx, renameErr, renameResult.MutationApplied, renameResult.MutationApplied)
	}
	if !renameResult.MutationApplied {
		return result, swapFailureError(protocol.StageWorkspaceSwap, errSwapNotApplied)
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.WithoutCancel(ctx),
		swapCleanupTimeout,
	)
	defer cancelCleanup()
	cleanupTransaction := swapTransaction
	cleanupTransaction.Stage = protocol.StageWorkspaceCleanup
	reportSwapStage(request, protocol.StageWorkspaceCleanup)
	if err := s.transactions.WriteTransaction(cleanupCtx, state.TransactionUpdate, cleanupTransaction); err != nil {
		return result, mapCommittedTransactionWriteFailure(protocol.StageWorkspaceCleanup, err)
	}
	if err := request.CommitEnvironment(cleanupCtx, request.Revision); err != nil {
		return result, mapCommittedEnvironmentFailure(err)
	}
	reportSwapStage(request, protocol.StageWorkspaceCleanup)
	if repositoryExists {
		deleteResult, deleteErr := s.operator.RemoveTree(cleanupCtx, filesystem.DeleteRequest{
			Kind:             filesystem.DeleteRepositoryRetired,
			Target:           previousPath,
			OperationID:      request.Transaction.OperationID,
			Reason:           swapCleanupReason,
			ExpectedIdentity: request.ActiveIdentity,
		})
		if deleteErr != nil || deleteResult.Partial || !deleteResult.AuditCompleted {
			return result, mapCommittedRetiredCleanupFailure(deleteResult, deleteErr)
		}
	}
	result.CleanupCompleted = true
	return result, nil
}

func validateSwapRequest(request SwapRequest) error {
	if err := state.ValidateTransaction(state.TransactionUpdate, request.Transaction); err != nil {
		return fmt.Errorf("%w: transaction: %w", errInvalidSwapRequest, err)
	}
	if request.Transaction.Stage != protocol.StageWorkspaceVerify ||
		request.Revision.validate() != nil ||
		request.Transaction.TargetVersion != request.Revision.Version() ||
		request.CommitEnvironment == nil {
		return errInvalidSwapRequest
	}
	return nil
}

func mapCommittedEnvironmentFailure(cause error) *Error {
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
	return newCommittedError(
		protocol.CodeStateWriteFailed,
		protocol.StageWorkspaceSwap,
		messageForCode(protocol.CodeStateWriteFailed),
		map[string]any{},
		cause,
	)
}

func pathExists(ctx context.Context, layout *config.Layout, path string) (bool, error) {
	inspection, err := filesystem.InspectManagedDirectory(ctx, layout, path)
	if err != nil {
		return false, fmt.Errorf("inspect repository path: %w", err)
	}
	return inspection.Exists, nil
}

func mapRenameFailure(
	ctx context.Context,
	cause error,
	mutationApplied bool,
	committed bool,
) *Error {
	if committed {
		return newCommittedError(
			protocol.CodeGitRepoSwapFailed,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeGitRepoSwapFailed),
			map[string]any{},
			cause,
		)
	}
	// 系统调用后的取消不能覆盖已落盘的 mutation；提交点前交由 Recovery 收口。
	if mutationApplied {
		if isAmbiguousFilesystemError(cause) {
			return newError(
				protocol.CodeUpdateStateAmbiguous,
				protocol.StageWorkspaceSwap,
				messageForCode(protocol.CodeUpdateStateAmbiguous),
				map[string]any{"reason": "directory_identity_changed"},
				cause,
			)
		}
		return swapFailureError(protocol.StageWorkspaceSwap, cause)
	}
	if isCancellation(ctx, cause) {
		return newError(
			protocol.CodeOperationCancelled,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeOperationCancelled),
			map[string]any{},
			cause,
		)
	}
	if isAmbiguousFilesystemError(cause) {
		return newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{"reason": "directory_identity_changed"},
			cause,
		)
	}
	var coded interface{ Code() protocol.Code }
	if errors.As(cause, &coded) && coded.Code() == protocol.CodeDirectoryOccupied ||
		errors.Is(cause, filesystem.ErrDestinationExists) {
		return newError(
			protocol.CodeDirectoryOccupied,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeDirectoryOccupied),
			map[string]any{},
			cause,
		)
	}
	return swapFailureError(protocol.StageWorkspaceSwap, cause)
}

func mapTransactionWriteFailure(
	ctx context.Context,
	stage protocol.Stage,
	cause error,
) *Error {
	var writeErr *state.WriteError
	if !errors.As(cause, &writeErr) && isCancellation(ctx, cause) {
		return newError(
			protocol.CodeOperationCancelled,
			stage,
			messageForCode(protocol.CodeOperationCancelled),
			map[string]any{},
			cause,
		)
	}
	return newError(
		protocol.CodeStateWriteFailed,
		stage,
		messageForCode(protocol.CodeStateWriteFailed),
		map[string]any{},
		cause,
	)
}

func mapCommittedTransactionWriteFailure(stage protocol.Stage, cause error) *Error {
	return newCommittedError(
		protocol.CodeStateWriteFailed,
		stage,
		messageForCode(protocol.CodeStateWriteFailed),
		map[string]any{},
		cause,
	)
}

func mapRetiredCleanupFailure(
	ctx context.Context,
	result filesystem.DeleteResult,
	cause error,
) *Error {
	if isCancellation(ctx, cause) {
		return newError(
			protocol.CodeOperationCancelled,
			protocol.StageWorkspaceCleanup,
			messageForCode(protocol.CodeOperationCancelled),
			map[string]any{},
			cause,
		)
	}
	if cause == nil {
		cause = fmt.Errorf(
			"retired repository cleanup incomplete: partial=%t audit=%t",
			result.Partial,
			result.AuditCompleted,
		)
	}
	return newError(
		protocol.CodeGitRepoCleanupFailed,
		protocol.StageWorkspaceCleanup,
		messageForCode(protocol.CodeGitRepoCleanupFailed),
		map[string]any{},
		cause,
	)
}

func mapCommittedRetiredCleanupFailure(
	result filesystem.DeleteResult,
	cause error,
) *Error {
	return mapCommittedCleanupFailure(result, cause)
}

func mapCommittedCleanupFailure(
	result filesystem.DeleteResult,
	cause error,
) *Error {
	if cause == nil {
		cause = fmt.Errorf(
			"repository cleanup incomplete: partial=%t audit=%t",
			result.Partial,
			result.AuditCompleted,
		)
	}
	return newCommittedError(
		protocol.CodeGitRepoCleanupFailed,
		protocol.StageWorkspaceCleanup,
		messageForCode(protocol.CodeGitRepoCleanupFailed),
		map[string]any{},
		cause,
	)
}

func isCancellation(ctx context.Context, cause error) bool {
	return ctx != nil && ctx.Err() != nil ||
		errors.Is(cause, context.Canceled) ||
		errors.Is(cause, context.DeadlineExceeded)
}

func isAmbiguousFilesystemError(cause error) bool {
	if cause == nil {
		return false
	}
	if errors.Is(cause, filesystem.ErrIdentityChanged) ||
		errors.Is(cause, filesystem.ErrUnsupportedCaseSensitivity) {
		return true
	}
	var coded interface{ Code() protocol.Code }
	if !errors.As(cause, &coded) {
		return false
	}
	switch coded.Code() {
	case protocol.CodeUnsafeReparsePoint, protocol.CodePathOutsideManagedRoot:
		return true
	default:
		return false
	}
}

func swapFailureError(stage protocol.Stage, cause error) *Error {
	return newError(
		protocol.CodeGitRepoSwapFailed,
		stage,
		messageForCode(protocol.CodeGitRepoSwapFailed),
		map[string]any{},
		cause,
	)
}

func swapInternalError(cause error) *Error {
	return newError(
		protocol.CodeInternalError,
		protocol.StageWorkspaceSwap,
		messageForCode(protocol.CodeInternalError),
		map[string]any{},
		cause,
	)
}

func swapCancelledError(cause error) *Error {
	return newError(
		protocol.CodeOperationCancelled,
		protocol.StageWorkspaceSwap,
		messageForCode(protocol.CodeOperationCancelled),
		map[string]any{},
		cause,
	)
}

func reportSwapStage(request SwapRequest, stage protocol.Stage) {
	if request.StageReporter != nil {
		request.StageReporter(stage)
	}
}

var (
	_ repositorySwapOperator  = (*filesystem.Operator)(nil)
	_ updateTransactionWriter = (*state.Store)(nil)
)
