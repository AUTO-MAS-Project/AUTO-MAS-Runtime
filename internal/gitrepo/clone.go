package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitcache "github.com/go-git/go-git/v5/plumbing/cache"
	gitstorage "github.com/go-git/go-git/v5/storage"
	gitfilesystem "github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	maxCloneProgressPulses = 64
	cloneCleanupTimeout    = 30 * time.Second

	cloneProgressStartMessage     = "正在获取后端仓库"
	cloneProgressPulseMessage     = "正在接收后端仓库数据"
	cloneProgressSuccessMessage   = "后端仓库获取完成"
	cloneProgressFailureMessage   = "后端仓库获取失败"
	cloneProgressCancelledMessage = "后端仓库获取已取消"
	cloneCleanupReason            = "git-fetch-attempt"

	failureResolve           mirror.FailureKind = "resolve_failed"
	failureBranchMissing     mirror.FailureKind = "branch_not_found"
	failureClone             mirror.FailureKind = "clone_failed"
	failureRepositoryInvalid mirror.FailureKind = "repository_invalid"
	failureVersionMismatch   mirror.FailureKind = "version_mismatch"
	failureCleanup           mirror.FailureKind = "cleanup_failed"
	failureOutput            mirror.FailureKind = "output_failed"
	failureCancelled         mirror.FailureKind = "cancelled"
	failureVerifierContract  mirror.FailureKind = "verifier_contract"
)

var ErrInvalidFetcher = errors.New("git fetcher is invalid")

// FetchRequest 固定一次 Git 获取操作的目标、镜像计划和目录所有者。
type FetchRequest struct {
	Plan          mirror.Plan
	Target        Target
	OperationID   string
	StageReporter StageReporter
}

// FetchResult 保存已验证临时仓库的安全路径和最终来源事实。
type FetchResult struct {
	RepositoryPath string
	Revision       Revision
}

type rotationRunner interface {
	Run(
		ctx context.Context,
		plan mirror.Plan,
		target mirror.Target,
		attempt mirror.AttemptFunc,
	) (mirror.RotationResult, error)
}

type gitClient interface {
	ListReferences(
		ctx context.Context,
		sourceURL string,
		caBundle []byte,
	) ([]*plumbing.Reference, error)
	Clone(ctx context.Context, path string, options git.CloneOptions) error
}

type treeRemover interface {
	RemoveTree(
		ctx context.Context,
		request filesystem.DeleteRequest,
	) (filesystem.DeleteResult, error)
}

type directoryLease interface {
	Close() error
}

type directoryPreparer func(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (directoryLease, error)

type cloneVerifier interface {
	Verify(
		ctx context.Context,
		request VerificationRequest,
	) (Revision, error)
}

// ProgressEmitter 是 Fetcher 消费的单方法协议进度出口。
type ProgressEmitter interface {
	EmitProgress(event protocol.ProgressEvent) error
}

type progressEmitter func(event protocol.ProgressEvent) error

type cleanupContextFactory func(
	ctx context.Context,
) (context.Context, context.CancelFunc)

type fetcherDependencies struct {
	layout         *config.Layout
	rotator        rotationRunner
	git            gitClient
	remover        treeRemover
	verifier       cloneVerifier
	prepareDir     directoryPreparer
	emitProgress   progressEmitter
	caBundle       []byte
	cleanupContext cleanupContextFactory
}

// Fetcher 获取并验证一个固定目标分支，不负责激活仓库目录。
type Fetcher struct {
	layout         *config.Layout
	rotator        rotationRunner
	git            gitClient
	remover        treeRemover
	verifier       cloneVerifier
	prepareDir     directoryPreparer
	emitProgress   progressEmitter
	caBundle       []byte
	cleanupContext cleanupContextFactory
}

// NewFetcher 创建使用 go-git 传输和真实静态校验器的仓库获取器。
func NewFetcher(
	layout *config.Layout,
	rotator *mirror.Rotator,
	remover *filesystem.Operator,
	verifier *Verifier,
	emitter ProgressEmitter,
) (*Fetcher, error) {
	if nilDependency(emitter) {
		return nil, ErrInvalidFetcher
	}
	return newFetcherWithDependencies(fetcherDependencies{
		layout:       layout,
		rotator:      rotator,
		git:          goGitClient{},
		remover:      remover,
		verifier:     verifier,
		prepareDir:   prepareManagedDirectoryLease,
		emitProgress: emitter.EmitProgress,
	})
}

func newFetcherWithDependencies(dependencies fetcherDependencies) (*Fetcher, error) {
	if dependencies.cleanupContext == nil {
		dependencies.cleanupContext = newCloneCleanupContext
	}
	if dependencies.layout == nil ||
		nilDependency(dependencies.rotator) ||
		nilDependency(dependencies.git) ||
		nilDependency(dependencies.remover) ||
		nilDependency(dependencies.verifier) ||
		dependencies.emitProgress == nil ||
		dependencies.cleanupContext == nil {
		return nil, ErrInvalidFetcher
	}
	return &Fetcher{
		layout:         dependencies.layout,
		rotator:        dependencies.rotator,
		git:            dependencies.git,
		remover:        dependencies.remover,
		verifier:       dependencies.verifier,
		prepareDir:     dependencies.prepareDir,
		emitProgress:   dependencies.emitProgress,
		caBundle:       append([]byte(nil), dependencies.caBundle...),
		cleanupContext: dependencies.cleanupContext,
	}, nil
}

// Fetch 按镜像计划获取同一 Target，并只返回通过静态校验的临时仓库。
func (f *Fetcher) Fetch(ctx context.Context, request FetchRequest) (FetchResult, error) {
	if ctx == nil || !validFetcher(f) {
		return FetchResult{}, newError(
			protocol.CodeInternalError,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeInternalError),
			map[string]any{},
			ErrInvalidFetcher,
		)
	}
	if err := request.Target.validate(); err != nil {
		return FetchResult{}, newError(
			protocol.CodeInvalidVersion,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeInvalidVersion),
			map[string]any{},
			err,
		)
	}
	reportFetchStage(request, protocol.StageWorkspaceClone)
	if err := ctx.Err(); err != nil {
		return FetchResult{}, f.cancelledError(err)
	}
	repositoryPath, err := f.layout.RepoUpdateDir(request.OperationID)
	if err != nil {
		return FetchResult{}, newError(
			protocol.CodeInternalError,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeInternalError),
			map[string]any{},
			fmt.Errorf("derive repository update path: %w", err),
		)
	}
	if err := requireTemporaryAbsent(ctx, f.layout, repositoryPath); err != nil {
		if isCancellation(ctx, err) {
			return FetchResult{}, f.cancelledError(err)
		}
		return FetchResult{}, newError(
			protocol.CodeUpdateStateAmbiguous,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeUpdateStateAmbiguous),
			map[string]any{},
			err,
		)
	}
	mirrorTarget, err := mirror.NewTarget(mirror.TargetSpec{
		ProductVersion: request.Target.Version(),
		ReleaseBranch:  request.Target.Branch(),
	})
	if err != nil {
		return FetchResult{}, newError(
			protocol.CodeInternalError,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeInternalError),
			map[string]any{},
			fmt.Errorf("build mirror target: %w", err),
		)
	}

	rotationResult, err := f.rotator.Run(
		ctx,
		request.Plan,
		mirrorTarget,
		func(attemptCtx context.Context, attempt mirror.Attempt) mirror.AttemptOutcome {
			return f.fetchAttempt(attemptCtx, request, repositoryPath, attempt)
		},
	)
	if err != nil {
		return FetchResult{}, mapFetchFailure(ctx, err, rotationResult.Reports)
	}
	revision, err := newRevision(
		request.Target,
		rotationResult.ActualCommit,
		rotationResult.Source,
	)
	if err != nil {
		return FetchResult{}, newError(
			protocol.CodeInternalError,
			protocol.StageWorkspaceVerify,
			messageForCode(protocol.CodeInternalError),
			map[string]any{},
			fmt.Errorf("build fetched revision: %w", err),
		)
	}
	return FetchResult{
		RepositoryPath: repositoryPath,
		Revision:       revision,
	}, nil
}

func (f *Fetcher) fetchAttempt(
	ctx context.Context,
	request FetchRequest,
	repositoryPath string,
	attempt mirror.Attempt,
) mirror.AttemptOutcome {
	reportFetchStage(request, protocol.StageWorkspaceClone)
	if attempt.Target.ProductVersion() != request.Target.Version() ||
		attempt.Target.ReleaseBranch() != request.Target.Branch() {
		return failedOutcome(
			mirror.OutcomeTargetFailure,
			failureVerifierContract,
			newError(
				protocol.CodeInternalError,
				protocol.StageWorkspaceClone,
				messageForCode(protocol.CodeInternalError),
				map[string]any{},
				ErrInvalidFetcher,
			),
		)
	}
	if err := f.emit(protocol.ProgressRunning, cloneProgressStartMessage); err != nil {
		return failedOutcome(
			mirror.OutcomeTargetFailure,
			failureOutput,
			f.outputError(err),
		)
	}

	references, err := f.git.ListReferences(
		ctx,
		attempt.Source.BaseURL(),
		append([]byte(nil), f.caBundle...),
	)
	if ctxErr := ctx.Err(); ctxErr != nil || isCancellation(ctx, err) {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureCancelled,
			f.cancelledError(errors.Join(ctxErr, err)),
			false,
		)
	}
	if err != nil {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeRetrySameSource,
			failureResolve,
			newError(
				protocol.CodeGitRemoteResolveFailed,
				protocol.StageWorkspaceClone,
				messageForCode(protocol.CodeGitRemoteResolveFailed),
				map[string]any{},
				err,
			),
			false,
		)
	}
	if !containsBranch(references, request.Target.Branch()) {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeSwitchSource,
			failureBranchMissing,
			newError(
				protocol.CodeGitBranchNotFound,
				protocol.StageWorkspaceClone,
				messageForCode(protocol.CodeGitBranchNotFound),
				map[string]any{},
				errors.New("target branch is absent"),
			),
			false,
		)
	}
	var lease directoryLease
	if f.prepareDir != nil {
		lease, err = f.prepareDir(ctx, f.layout, repositoryPath)
		if ctxErr := ctx.Err(); ctxErr != nil || isCancellation(ctx, err) {
			return f.finishFailedAttempt(
				ctx,
				request,
				repositoryPath,
				mirror.OutcomeTargetFailure,
				failureCancelled,
				f.cancelledError(errors.Join(ctxErr, err)),
				false,
				lease,
			)
		}
		if err != nil {
			return f.finishFailedAttempt(
				ctx,
				request,
				repositoryPath,
				mirror.OutcomeTargetFailure,
				failureVerifierContract,
				newError(
					protocol.CodeUpdateStateAmbiguous,
					protocol.StageWorkspaceClone,
					messageForCode(protocol.CodeUpdateStateAmbiguous),
					map[string]any{},
					err,
				),
				false,
				lease,
			)
		}
	}

	progress := newCloneProgressWriter(f.emitProgress)
	cloneErr := f.git.Clone(ctx, repositoryPath, git.CloneOptions{
		URL:               attempt.Source.BaseURL(),
		RemoteName:        "origin",
		ReferenceName:     plumbing.NewBranchReferenceName(request.Target.Branch()),
		SingleBranch:      true,
		Depth:             1,
		RecurseSubmodules: git.NoRecurseSubmodules,
		Progress:          progress,
		Tags:              git.NoTags,
		InsecureSkipTLS:   false,
		CABundle:          append([]byte(nil), f.caBundle...),
	})
	if progressErr := progress.Err(); progressErr != nil {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureOutput,
			f.outputError(errors.Join(progressErr, cloneErr)),
			true,
			lease,
		)
	}
	if ctxErr := ctx.Err(); ctxErr != nil || isCancellation(ctx, cloneErr) {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureCancelled,
			f.cancelledError(errors.Join(ctxErr, cloneErr)),
			true,
			lease,
		)
	}
	if errors.Is(cloneErr, git.ErrRepositoryAlreadyExists) {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureVerifierContract,
			newError(
				protocol.CodeUpdateStateAmbiguous,
				protocol.StageWorkspaceClone,
				messageForCode(protocol.CodeUpdateStateAmbiguous),
				map[string]any{},
				cloneErr,
			),
			false,
			lease,
		)
	}
	if cloneErr != nil {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeRetrySameSource,
			failureClone,
			newError(
				protocol.CodeGitCloneFailed,
				protocol.StageWorkspaceClone,
				messageForCode(protocol.CodeGitCloneFailed),
				map[string]any{},
				cloneErr,
			),
			true,
			lease,
		)
	}

	reportFetchStage(request, protocol.StageWorkspaceVerify)
	verification, verifyErr := f.verifier.Verify(ctx, VerificationRequest{
		RepositoryPath: repositoryPath,
		Target:         request.Target,
		Source:         attempt.Source,
		AllowedSources: request.Plan.Sources(),
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureCancelled,
			f.cancelledError(errors.Join(ctxErr, verifyErr)),
			true,
			lease,
		)
	}
	if verifyErr != nil {
		code := protocol.CodeGitRepositoryInvalid
		kind := failureRepositoryInvalid
		var operationErr *Error
		if errors.As(verifyErr, &operationErr) && operationErr.Code() == protocol.CodeGitVersionMismatch {
			code = protocol.CodeGitVersionMismatch
			kind = failureVersionMismatch
		}
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeIntegrityFailure,
			kind,
			newError(
				code,
				protocol.StageWorkspaceVerify,
				messageForCode(code),
				map[string]any{},
				verifyErr,
			),
			true,
			lease,
		)
	}
	if err := verification.validate(); err != nil ||
		verification.Version() != request.Target.Version() ||
		verification.Branch() != request.Target.Branch() ||
		verification.SourceKey() != attempt.Source.Key() {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureVerifierContract,
			newError(
				protocol.CodeInternalError,
				protocol.StageWorkspaceVerify,
				messageForCode(protocol.CodeInternalError),
				map[string]any{},
				ErrInvalidFetcher,
			),
			true,
			lease,
		)
	}
	if lease != nil {
		if closeErr := lease.Close(); closeErr != nil {
			return f.finishFailedAttempt(
				ctx,
				request,
				repositoryPath,
				mirror.OutcomeTargetFailure,
				failureCleanup,
				newError(
					protocol.CodeGitRepoCleanupFailed,
					protocol.StageWorkspaceCleanup,
					messageForCode(protocol.CodeGitRepoCleanupFailed),
					map[string]any{},
					closeErr,
				),
				true,
				lease,
			)
		}
		lease = nil
	}
	if err := f.emit(protocol.ProgressSucceeded, cloneProgressSuccessMessage); err != nil {
		return f.finishFailedAttempt(
			ctx,
			request,
			repositoryPath,
			mirror.OutcomeTargetFailure,
			failureOutput,
			f.outputError(err),
			true,
			lease,
		)
	}
	return mirror.AttemptOutcome{
		Kind:         mirror.OutcomeSucceeded,
		ActualCommit: verification.Commit(),
	}
}

func (f *Fetcher) finishFailedAttempt(
	ctx context.Context,
	request FetchRequest,
	repositoryPath string,
	outcomeKind mirror.OutcomeKind,
	failureKind mirror.FailureKind,
	operationErr *Error,
	cleanup bool,
	leaseValues ...directoryLease,
) mirror.AttemptOutcome {
	var lease directoryLease
	if len(leaseValues) > 0 {
		lease = leaseValues[0]
	}
	if lease != nil {
		if closeErr := lease.Close(); closeErr != nil {
			if operationErr.Code() == protocol.CodeOutputWriteFailed ||
				operationErr.Code() == protocol.CodeOperationCancelled {
				operationErr = newError(
					operationErr.Code(),
					operationErr.Stage(),
					operationErr.Message(),
					operationErr.Details(),
					errors.Join(operationErr, closeErr),
				)
			} else {
				operationErr = newError(
					protocol.CodeGitRepoCleanupFailed,
					protocol.StageWorkspaceCleanup,
					messageForCode(protocol.CodeGitRepoCleanupFailed),
					map[string]any{},
					errors.Join(operationErr, closeErr),
				)
				failureKind = failureCleanup
			}
			outcomeKind = mirror.OutcomeTargetFailure
			cleanup = true
		}
	}
	status := protocol.ProgressFailed
	message := cloneProgressFailureMessage
	if operationErr.Code() == protocol.CodeOperationCancelled {
		status = protocol.ProgressCancelled
		message = cloneProgressCancelledMessage
	}
	if operationErr.Code() != protocol.CodeOutputWriteFailed {
		if emitErr := f.emit(status, message); emitErr != nil {
			operationErr = f.outputError(errors.Join(operationErr, emitErr))
			outcomeKind = mirror.OutcomeTargetFailure
			failureKind = failureOutput
		}
	}
	if cleanup {
		reportFetchStage(request, protocol.StageWorkspaceCleanup)
		if cleanupErr := f.cleanupTemporary(ctx, request, repositoryPath); cleanupErr != nil {
			if operationErr.Code() == protocol.CodeOutputWriteFailed ||
				operationErr.Code() == protocol.CodeOperationCancelled {
				operationErr = newError(
					operationErr.Code(),
					operationErr.Stage(),
					operationErr.Message(),
					operationErr.Details(),
					errors.Join(operationErr, cleanupErr),
				)
			} else {
				operationErr = newError(
					protocol.CodeGitRepoCleanupFailed,
					protocol.StageWorkspaceCleanup,
					messageForCode(protocol.CodeGitRepoCleanupFailed),
					map[string]any{},
					errors.Join(operationErr, cleanupErr),
				)
				failureKind = failureCleanup
			}
			outcomeKind = mirror.OutcomeTargetFailure
		}
	}
	return failedOutcome(outcomeKind, failureKind, operationErr)
}

func (f *Fetcher) cleanupTemporary(
	ctx context.Context,
	request FetchRequest,
	repositoryPath string,
) error {
	cleanupCtx, cancel := f.cleanupContext(ctx)
	if cleanupCtx == nil || cancel == nil {
		return ErrInvalidFetcher
	}
	defer cancel()
	result, err := f.remover.RemoveTree(cleanupCtx, filesystem.DeleteRequest{
		Kind:        filesystem.DeleteRepositoryUpdate,
		Target:      repositoryPath,
		OperationID: request.OperationID,
		Reason:      cloneCleanupReason,
	})
	if err != nil {
		return fmt.Errorf("remove repository update: %w", err)
	}
	if result.Partial || !result.AuditCompleted {
		return errors.New("repository update cleanup is incomplete")
	}
	return nil
}

func (f *Fetcher) emit(status protocol.ProgressStatus, message string) error {
	return f.emitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageWorkspaceClone,
		Status:  status,
		Message: message,
	})
}

func (f *Fetcher) outputError(cause error) *Error {
	return newError(
		protocol.CodeOutputWriteFailed,
		protocol.StageWorkspaceClone,
		messageForCode(protocol.CodeOutputWriteFailed),
		map[string]any{},
		cause,
	)
}

func (f *Fetcher) cancelledError(cause error) *Error {
	return newError(
		protocol.CodeOperationCancelled,
		protocol.StageWorkspaceClone,
		messageForCode(protocol.CodeOperationCancelled),
		map[string]any{},
		cause,
	)
}

func mapFetchFailure(
	ctx context.Context,
	cause error,
	reports []mirror.AttemptReport,
) *Error {
	details := attemptDetails(reports)
	priority := []protocol.Code{
		protocol.CodeOutputWriteFailed,
		protocol.CodeUpdateStateAmbiguous,
		protocol.CodeGitRepoCleanupFailed,
		protocol.CodeGitVersionMismatch,
		protocol.CodeGitRepositoryInvalid,
		protocol.CodeGitCloneFailed,
		protocol.CodeGitRemoteResolveFailed,
		protocol.CodeGitBranchNotFound,
	}
	if hasErrorCode(cause, protocol.CodeOutputWriteFailed) {
		return newError(
			protocol.CodeOutputWriteFailed,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeOutputWriteFailed),
			details,
			cause,
		)
	}
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return newError(
			protocol.CodeOperationCancelled,
			protocol.StageWorkspaceClone,
			messageForCode(protocol.CodeOperationCancelled),
			details,
			cause,
		)
	}
	for _, code := range priority[1:] {
		if hasErrorCode(cause, code) {
			stage := protocol.StageWorkspaceClone
			if code == protocol.CodeGitRepoCleanupFailed {
				stage = protocol.StageWorkspaceCleanup
			} else if code == protocol.CodeGitVersionMismatch || code == protocol.CodeGitRepositoryInvalid {
				stage = protocol.StageWorkspaceVerify
			}
			return newError(code, stage, messageForCode(code), details, cause)
		}
	}
	var rotationErr *mirror.RotationError
	if errors.As(cause, &rotationErr) {
		code := rotationErr.Code()
		if code == protocol.CodeNetworkUnavailable || code == protocol.CodeMirrorExhausted {
			return newError(code, protocol.StageWorkspaceClone, messageForCode(code), details, cause)
		}
	}
	return newError(
		protocol.CodeInternalError,
		protocol.StageWorkspaceClone,
		messageForCode(protocol.CodeInternalError),
		details,
		cause,
	)
}

func attemptDetails(reports []mirror.AttemptReport) map[string]any {
	attempts := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		attempts = append(attempts, map[string]any{
			"source":      report.SourceKey,
			"sourceTry":   report.SourceTry,
			"globalTry":   report.GlobalTry,
			"outcome":     report.Outcome.String(),
			"failureKind": report.FailureKind.String(),
		})
	}
	return map[string]any{"attempts": attempts}
}

func hasErrorCode(err error, code protocol.Code) bool {
	if err == nil {
		return false
	}
	if operationErr, ok := err.(*Error); ok && operationErr.Code() == code {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, nested := range wrapped.Unwrap() {
			if hasErrorCode(nested, code) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return hasErrorCode(wrapped.Unwrap(), code)
	}
	return false
}

func failedOutcome(
	kind mirror.OutcomeKind,
	failureKind mirror.FailureKind,
	err error,
) mirror.AttemptOutcome {
	return mirror.AttemptOutcome{
		Kind:        kind,
		FailureKind: failureKind,
		Err:         err,
	}
}

func containsBranch(references []*plumbing.Reference, branch string) bool {
	want := plumbing.NewBranchReferenceName(branch)
	for _, reference := range references {
		if reference != nil && reference.Name() == want {
			return true
		}
	}
	return false
}

func requireTemporaryAbsent(
	ctx context.Context,
	layout *config.Layout,
	path string,
) error {
	if ctx == nil {
		return fmt.Errorf("inspect repository update path: %w", ErrInvalidFetcher)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	inspection, err := filesystem.InspectManagedDirectory(ctx, layout, path)
	if err != nil {
		return fmt.Errorf("inspect repository update path: %w", err)
	}
	if inspection.Exists {
		return errors.New("repository update path already exists")
	}
	return nil
}

func prepareManagedDirectoryLease(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (directoryLease, error) {
	return filesystem.PrepareManagedDirectory(ctx, layout, path)
}

func validCommit(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for i := 0; i < len(commit); i++ {
		character := commit[i]
		if (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func newCloneCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cloneCleanupTimeout)
}

func validFetcher(fetcher *Fetcher) bool {
	return fetcher != nil &&
		fetcher.layout != nil &&
		!nilDependency(fetcher.rotator) &&
		!nilDependency(fetcher.git) &&
		!nilDependency(fetcher.remover) &&
		!nilDependency(fetcher.verifier) &&
		fetcher.emitProgress != nil &&
		fetcher.cleanupContext != nil
}

func reportFetchStage(request FetchRequest, stage protocol.Stage) {
	if request.StageReporter != nil {
		request.StageReporter(stage)
	}
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type cloneProgressWriter struct {
	mu      sync.Mutex
	emit    progressEmitter
	pulses  int
	lastErr error
}

func newCloneProgressWriter(emit progressEmitter) *cloneProgressWriter {
	return &cloneProgressWriter{emit: emit}
}

func (w *cloneProgressWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastErr != nil {
		return 0, w.lastErr
	}
	if w.pulses >= maxCloneProgressPulses {
		return len(payload), nil
	}
	if w.emit == nil {
		w.lastErr = ErrInvalidFetcher
		return 0, w.lastErr
	}
	if err := w.emit(protocol.ProgressEvent{
		Stage:   protocol.StageWorkspaceClone,
		Status:  protocol.ProgressRunning,
		Message: cloneProgressPulseMessage,
	}); err != nil {
		w.lastErr = fmt.Errorf("emit clone progress: %w", err)
		return 0, w.lastErr
	}
	w.pulses++
	return len(payload), nil
}

func (w *cloneProgressWriter) Err() error {
	if w == nil {
		return ErrInvalidFetcher
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

type goGitClient struct{}

// cloneStorage 只暴露 go-git 必需的 Storer 方法，避免触发有截断 pack 句柄缺陷的快捷写入路径。
type cloneStorage struct {
	gitstorage.Storer
	init func() error
}

func (s cloneStorage) Init() error {
	return s.init()
}

func (goGitClient) ListReferences(
	ctx context.Context,
	sourceURL string,
	caBundle []byte,
) ([]*plumbing.Reference, error) {
	remote := git.NewRemote(memory.NewStorage(), &gitcfg.RemoteConfig{
		Name: "origin",
		URLs: []string{sourceURL},
	})
	return remote.ListContext(ctx, &git.ListOptions{
		InsecureSkipTLS: false,
		CABundle:        append([]byte(nil), caBundle...),
	})
}

func (goGitClient) Clone(
	ctx context.Context,
	path string,
	options git.CloneOptions,
) error {
	worktree := osfs.New(path)
	gitDirectory, err := worktree.Chroot(git.GitDirName)
	if err != nil {
		return fmt.Errorf("create git directory: %w", err)
	}
	diskStore := gitfilesystem.NewStorage(gitDirectory, gitcache.NewObjectLRUDefault())
	store := cloneStorage{
		Storer: diskStore,
		init:   diskStore.Init,
	}
	_, err = git.CloneContext(ctx, store, worktree, &options)
	return err
}

var _ gitClient = goGitClient{}
