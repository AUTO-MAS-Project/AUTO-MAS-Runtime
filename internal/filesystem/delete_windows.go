package filesystem

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// New 创建受管删除与原子重命名操作器。
func New(
	ctx context.Context,
	layout *config.Layout,
	auditor Auditor,
	optionValues ...Option,
) (*Operator, error) {
	configured := options{
		wait:   defaultWait,
		delays: defaultRenameDelays(),
	}
	for _, option := range optionValues {
		if option == nil {
			return nil, ErrInvalidArgument
		}
		if err := option(&configured); err != nil {
			return nil, fmt.Errorf("%w: operator option: %w", ErrInvalidArgument, err)
		}
	}
	return newWithDependencies(
		ctx,
		layout,
		auditor,
		configured,
		operatorDependencies{
			api: newProductionPathAPI(),
			finishedContext: func(
				operationCtx context.Context,
			) (context.Context, context.CancelFunc) {
				return context.WithTimeout(
					context.WithoutCancel(operationCtx),
					5*time.Second,
				)
			},
		},
	)
}

func newWithDependencies(
	ctx context.Context,
	layout *config.Layout,
	auditor Auditor,
	configured options,
	dependencies operatorDependencies,
) (*Operator, error) {
	if ctx == nil || layout == nil || auditor == nil ||
		!dependencies.api.valid() || configured.wait == nil ||
		len(configured.delays) == 0 || len(configured.delays) > 16 ||
		dependencies.finishedContext == nil {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, delay := range configured.delays {
		if delay <= 0 {
			return nil, ErrInvalidArgument
		}
	}
	targets := []string{
		layout.AppRoot(),
		layout.RepoDir(),
		layout.StateDir(),
		layout.RuntimeDir(),
		layout.RuntimeCacheDir(),
		layout.UVCacheDir(),
		layout.DownloadCacheDir(),
		layout.BuildCacheDir(),
		layout.LogsDir(),
		layout.RuntimeLogDir(),
	}
	targets = append(targets, layout.ProtectedRootDirs()...)
	for _, target := range targets {
		if err := validateExistingLayoutPathWith(
			ctx,
			layout,
			target,
			dependencies.api,
		); err != nil {
			return nil, err
		}
	}
	return &Operator{
		layout:          layout,
		auditor:         auditor,
		api:             dependencies.api,
		wait:            configured.wait,
		delays:          append([]time.Duration(nil), configured.delays...),
		finishedContext: dependencies.finishedContext,
	}, nil
}

type authorizedDelete struct {
	request DeleteRequest
	target  CanonicalPath
	exists  bool
	chain   *pinnedChain
	root    *pinnedObject
}

func (o *Operator) authorizeDeleteRequest(
	ctx context.Context,
	request DeleteRequest,
) (authorizedDelete, error) {
	if ctx == nil {
		return authorizedDelete{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return authorizedDelete{}, err
	}
	if !request.Kind.Valid() {
		return authorizedDelete{}, ErrInvalidArgument
	}
	if request.Target == "" ||
		validateAuditValue(request.OperationID) != nil ||
		validateAuditValue(request.Reason) != nil {
		return authorizedDelete{}, ErrInvalidArgument
	}
	if request.Kind == DeleteUVStaging {
		if request.Version == "" {
			return authorizedDelete{}, ErrInvalidArgument
		}
	} else if request.Version != "" {
		return authorizedDelete{}, ErrInvalidArgument
	}

	expectedPath, exact, err := o.expectedDeletePath(request)
	if err != nil {
		return authorizedDelete{}, err
	}
	target, err := canonicalizeContextWith(ctx, request.Target, o.api)
	if err != nil {
		return authorizedDelete{}, err
	}
	appRoot, err := canonicalizeContextWith(ctx, o.layout.AppRoot(), o.api)
	if err != nil {
		return authorizedDelete{}, err
	}
	if !appRoot.Contains(target) {
		return authorizedDelete{}, outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
	}
	if exact {
		expected, err := canonicalizeContextWith(ctx, expectedPath, o.api)
		if err != nil {
			return authorizedDelete{}, err
		}
		if !expected.Equal(target) {
			return authorizedDelete{}, outsidePathError(
				"authorize-delete",
				target.String(),
				ErrIdentityChanged,
			)
		}
	} else if err := o.authorizePythonCache(ctx, target); err != nil {
		return authorizedDelete{}, err
	}
	if err := o.rejectProtectedDeleteTarget(ctx, target); err != nil {
		return authorizedDelete{}, err
	}

	chain, exists, err := o.pinDeleteTarget(ctx, appRoot, target)
	if err != nil {
		return authorizedDelete{}, err
	}
	root := &chain.objects[len(chain.objects)-1]
	return authorizedDelete{
		request: request,
		target:  target,
		exists:  exists,
		chain:   chain,
		root:    root,
	}, nil
}

func validateAuditValue(value string) error {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") ||
		!utf8.ValidString(value) || utf8.RuneCountInString(value) > 512 {
		return ErrInvalidArgument
	}
	return nil
}

func (o *Operator) expectedDeletePath(
	request DeleteRequest,
) (string, bool, error) {
	switch request.Kind {
	case DeleteUVCache:
		return o.layout.UVCacheDir(), true, nil
	case DeleteManagedVenv:
		return o.layout.VenvDir(), true, nil
	case DeleteManagedPython:
		return o.layout.PythonDir(), true, nil
	case DeleteRepositoryUpdate:
		path, err := o.layout.RepoUpdateDir(request.OperationID)
		return path, true, wrapLayoutArgument(err)
	case DeleteRepositoryRetired:
		path, err := o.layout.RepoPreviousDir(request.OperationID)
		return path, true, wrapLayoutArgument(err)
	case DeleteDownloadTemporary:
		return o.layout.DownloadCacheDir(), true, nil
	case DeleteUVStaging:
		path, err := o.layout.UVStagingDir(request.Version, request.OperationID)
		return path, true, wrapLayoutArgument(err)
	case DeletePythonCache:
		return o.layout.RepoDir(), false, nil
	case DeleteBuildCache:
		return o.layout.BuildCacheDir(), true, nil
	default:
		return "", false, ErrInvalidArgument
	}
}

func wrapLayoutArgument(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: layout getter: %w", ErrInvalidArgument, err)
}

func (o *Operator) authorizePythonCache(
	ctx context.Context,
	target CanonicalPath,
) error {
	repo, err := canonicalizeContextWith(ctx, o.layout.RepoDir(), o.api)
	if err != nil {
		return err
	}
	if !repo.Contains(target) {
		return outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
	}
	result, err := compareStringOrdinal(filepath.Base(target.String()), "__pycache__", true)
	if err != nil || result != cstrEqual {
		return outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
	}
	return nil
}

func (o *Operator) rejectProtectedDeleteTarget(
	ctx context.Context,
	target CanonicalPath,
) error {
	exactRejected := []string{
		o.layout.AppRoot(),
		o.layout.RepoDir(),
	}
	for _, raw := range exactRejected {
		protected, err := canonicalizeContextWith(ctx, raw, o.api)
		if err != nil {
			return err
		}
		if protected.Equal(target) {
			return outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
		}
	}
	subtreeRejected := []string{
		o.layout.StateDir(),
		o.layout.LogsDir(),
	}
	subtreeRejected = append(subtreeRejected, o.layout.ProtectedRootDirs()...)
	for _, raw := range subtreeRejected {
		protected, err := canonicalizeContextWith(ctx, raw, o.api)
		if err != nil {
			return err
		}
		if protected.Equal(target) || protected.Contains(target) {
			return outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
		}
	}
	if isWindowsVolumeRoot(
		target.String(),
		filepath.VolumeName(target.String()),
	) {
		return outsidePathError("authorize-delete", target.String(), ErrIdentityChanged)
	}
	return nil
}

func (o *Operator) pinDeleteTarget(
	ctx context.Context,
	appRoot CanonicalPath,
	target CanonicalPath,
) (*pinnedChain, bool, error) {
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(o.layout.AppRoot()), o.api)
	if err != nil {
		return nil, false, err
	}
	_, attributeErr := o.api.attributes(target.Native())
	exists := attributeErr == nil
	if attributeErr != nil && !isWindowsNotFound(attributeErr) {
		return nil, false, &FileError{
			Operation: "attributes",
			Path:      target.String(),
			Err:       attributeErr,
		}
	}
	pinnedTarget := target
	if !exists {
		existing := filepath.Dir(target.String())
		for {
			_, err := o.api.attributes(nativeWindowsPath(existing))
			if err == nil {
				break
			}
			if !isWindowsNotFound(err) {
				return nil, false, &FileError{
					Operation: "attributes",
					Path:      existing,
					Err:       err,
				}
			}
			next := filepath.Dir(existing)
			if next == existing {
				return nil, false, outsidePathError(
					"authorize-delete",
					target.String(),
					ErrIdentityChanged,
				)
			}
			existing = next
		}
		pinnedTarget, err = canonicalizeContextWith(ctx, existing, o.api)
		if err != nil {
			return nil, false, err
		}
		if !appRoot.Equal(pinnedTarget) && !appRoot.Contains(pinnedTarget) {
			return nil, false, outsidePathError(
				"authorize-delete",
				target.String(),
				ErrIdentityChanged,
			)
		}
	}
	chain, err := openPinnedChainWith(
		ctx,
		rootParent,
		pinnedTarget,
		directoryPinSpec(),
		o.api,
	)
	if err != nil {
		return nil, false, err
	}
	return chain, exists, nil
}

func (o *Operator) recordDeleteStarted(
	ctx context.Context,
	delete authorizedDelete,
) error {
	err := o.auditor.RecordDeletion(ctx, DeleteAuditRecord{
		Phase:       DeleteAuditStarted,
		OperationID: delete.request.OperationID,
		Kind:        delete.request.Kind,
		Target:      delete.target.String(),
		Reason:      delete.request.Reason,
		Result:      "started",
	})
	if err != nil {
		return &AuditError{
			Phase:           DeleteAuditStarted,
			MutationApplied: false,
			Cause:           err,
		}
	}
	return nil
}
