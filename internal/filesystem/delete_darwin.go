package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// New 创建不持有永久 FD 的受控目录操作器，先验证已有布局链。
func New(ctx context.Context, layout *config.Layout, auditor Auditor, values ...Option) (*Operator, error) {
	if ctx == nil || layout == nil || auditor == nil {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	configured := options{wait: defaultWait, delays: defaultRenameDelays()}
	for _, value := range values {
		if value == nil {
			return nil, ErrInvalidArgument
		}
		if err := value(&configured); err != nil {
			return nil, fmt.Errorf("%w: operator option: %w", ErrInvalidArgument, err)
		}
	}
	root, err := openDarwinAbsoluteDirectory(ctx, layout.AppRoot())
	if err != nil {
		return nil, err
	}
	if err := root.file.Close(); err != nil {
		return nil, err
	}
	targets := append(layout.ProtectedRootDirs(), layout.RepoDir(), layout.StateDir(), layout.RuntimeDir(),
		layout.RuntimeCacheDir(), layout.UVCacheDir(), layout.DownloadCacheDir(), layout.BuildCacheDir(), layout.RuntimeLogDir())
	for _, target := range targets {
		if _, err := InspectManagedDirectory(ctx, layout, target); err != nil {
			return nil, err
		}
	}
	return &Operator{layout: layout, auditor: auditor, wait: configured.wait, delays: configured.delays,
		finishedContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		}}, nil
}

func (o *Operator) authorizeDarwinDelete(ctx context.Context, request DeleteRequest) (CanonicalPath, error) {
	if ctx == nil || o == nil || o.layout == nil || !request.Kind.Valid() || request.Target == "" ||
		validateAuditValue(request.OperationID) != nil || validateAuditValue(request.Reason) != nil {
		return CanonicalPath{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return CanonicalPath{}, err
	}
	needsVersion := request.Kind == DeleteUVStaging || request.Kind == DeleteUVVersion
	if needsVersion && request.Version == "" || !needsVersion && request.Version != "" {
		return CanonicalPath{}, ErrInvalidArgument
	}
	root, relative, err := darwinManagedRelative(ctx, o.layout, request.Target)
	if err != nil {
		return CanonicalPath{}, err
	}
	target := CanonicalPath{path: filepath.Join(root.path, relative)}
	expectedPath, exact, err := o.expectedDeletePath(request)
	if err != nil {
		return CanonicalPath{}, err
	}
	expected, err := Canonicalize(expectedPath)
	if err != nil {
		return CanonicalPath{}, err
	}
	if exact && !expected.Equal(target) || !exact && (!expected.Contains(target) || filepath.Base(target.path) != "__pycache__") {
		return CanonicalPath{}, darwinOutside("authorize-delete", target.path)
	}
	protected := append(o.layout.ProtectedRootDirs(), o.layout.StateDir(), o.layout.AppRoot(), o.layout.RepoDir())
	for _, path := range protected {
		boundary, err := Canonicalize(path)
		if err != nil {
			return CanonicalPath{}, err
		}
		rootOnly := path == o.layout.AppRoot() || path == o.layout.RepoDir()
		if boundary.Equal(target) || !rootOnly && boundary.Contains(target) {
			return CanonicalPath{}, darwinOutside("protect-delete", target.path)
		}
	}
	return target, nil
}

// RemoveTree 先证明整棵树的身份和类型，再删除已授权对象并准确报告部分副作用。
func (o *Operator) RemoveTree(ctx context.Context, request DeleteRequest) (DeleteResult, error) {
	target, err := o.authorizeDarwinDelete(ctx, request)
	if err != nil {
		return DeleteResult{}, err
	}
	parent, openErr := o.openDarwinDirectory(ctx, filepath.Dir(target.path))
	if openErr != nil {
		if isDarwinStableMissing(openErr) {
			if request.ExpectedIdentity != nil {
				return DeleteResult{}, ErrIdentityChanged
			}
			return o.auditDarwinRemoval(ctx, request, target.path, nil)
		}
		return DeleteResult{}, openErr
	}
	directory, openErr := parent.openDirectory(ctx, filepath.Base(target.path), false)
	if openErr != nil {
		closeErr := parent.file.Close()
		if isDarwinStableMissing(openErr) && closeErr == nil {
			if request.ExpectedIdentity != nil {
				return DeleteResult{}, ErrIdentityChanged
			}
			return o.auditDarwinRemoval(ctx, request, target.path, nil)
		}
		return DeleteResult{}, joinOperationCleanup(openErr, closeErr)
	}
	if request.ExpectedIdentity != nil && !request.ExpectedIdentity.Equal(&directory.identity) {
		return DeleteResult{}, errors.Join(ErrIdentityChanged, directory.file.Close(), parent.file.Close())
	}
	result, operationErr := o.auditDarwinRemoval(ctx, request, target.path, func(state *darwinRemoval) error {
		count := 1
		proof, err := scanDarwinTree(ctx, directory, 0, &count)
		if err != nil {
			return err
		}
		if err := removeDarwinChildren(ctx, directory, proof, state); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := parent.verifyName(filepath.Base(target.path), directory.identity, true); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(parent.file.Fd()), filepath.Base(target.path), unix.AT_REMOVEDIR); err != nil {
			return err
		}
		state.count++
		state.rootRemoved = true
		return parent.file.Sync()
	})
	return result, errors.Join(operationErr, directory.file.Close(), parent.file.Close())
}

func (o *Operator) auditDarwinRemoval(ctx context.Context, request DeleteRequest, target string, operation func(*darwinRemoval) error) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	record := DeleteAuditRecord{Phase: DeleteAuditStarted, OperationID: request.OperationID,
		Kind: request.Kind, Target: target, Reason: request.Reason}
	if err := o.auditor.RecordDeletion(ctx, record); err != nil {
		return DeleteResult{}, &AuditError{Phase: DeleteAuditStarted, Cause: err}
	}
	state := &darwinRemoval{}
	var operationErr error
	resultName := "not_found"
	if operation != nil {
		operationErr = operation(state)
		resultName = "succeeded"
		if operationErr != nil {
			resultName = "failed"
			if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
				resultName = "cancelled"
			}
		}
	}
	result := DeleteResult{Removed: state.count != 0, Partial: state.count != 0 && !state.rootRemoved && operationErr != nil}
	record.Phase = DeleteAuditFinished
	record.Removed, record.Partial, record.Result = result.Removed, result.Partial, resultName
	finishedCtx, cancel := o.finishedContext(ctx)
	finishedErr := o.auditor.RecordDeletion(finishedCtx, record)
	cancel()
	if finishedErr != nil {
		return result, errors.Join(operationErr, &AuditError{Phase: DeleteAuditFinished,
			MutationApplied: result.Removed, Cause: finishedErr})
	}
	result.AuditCompleted = true
	return result, operationErr
}
