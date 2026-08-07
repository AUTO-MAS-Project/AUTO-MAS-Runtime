package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

const managedDirectoryCloseAttempts = 3

func prepareManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	return prepareManagedDirectoryWithAPI(ctx, layout, path, newProductionPathAPI())
}

func pinManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	return pinManagedDirectoryWithAPI(ctx, layout, path, newProductionPathAPI())
}

func pinManagedDirectoryWithAPI(
	ctx context.Context,
	layout *config.Layout,
	path string,
	api pathAPI,
) (*DirectoryLease, error) {
	if ctx == nil || layout == nil || path == "" || !api.valid() {
		return nil, fmt.Errorf("%w: invalid managed-directory pin input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return nil, err
	}
	root, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return nil, err
	}
	if !root.Equal(target) && !root.Contains(target) {
		return nil, outsidePathError("pin-managed-directory-root", target.String(), ErrIdentityChanged)
	}
	if err := ensureLocalVolumeWith(root, api); err != nil {
		return nil, err
	}
	attributes, err := api.attributes(target.Native())
	if err != nil {
		if isWindowsNotFound(err) {
			if probeErr := rejectMissingReparseChain(ctx, target, api); probeErr != nil {
				return nil, probeErr
			}
		}
		return nil, &FileError{Operation: "attributes", Path: target.String(), Err: err}
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return nil, &FileError{Operation: "type", Path: target.String(), Err: ErrIdentityChanged}
	}
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(root.String()), api)
	if err != nil {
		return nil, err
	}
	chain, err := openPinnedChainWith(ctx, rootParent, target, directoryPinSpec(), api)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, chain.close())
	}
	leaf := chain.objects[len(chain.objects)-1]
	return newDirectoryLeaseWithIdentity(target.String(), leaf.identity, chain.close), nil
}

func prepareManagedDirectoryWithAPI(
	ctx context.Context,
	layout *config.Layout,
	path string,
	api pathAPI,
) (*DirectoryLease, error) {
	if ctx == nil || layout == nil || path == "" || !api.valid() {
		return nil, fmt.Errorf("%w: invalid managed-directory lease input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return nil, err
	}
	root, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return nil, err
	}
	if !root.Contains(target) || root.Equal(target) {
		return nil, outsidePathError("prepare-directory-root", target.String(), ErrIdentityChanged)
	}
	if err := ensureLocalVolumeWith(root, api); err != nil {
		return nil, err
	}

	parentPath := filepath.Dir(target.String())
	parent, err := canonicalizeContextWith(ctx, parentPath, api)
	if err != nil {
		return nil, err
	}
	if !root.Equal(parent) && !root.Contains(parent) {
		return nil, outsidePathError("prepare-directory-parent", target.String(), ErrIdentityChanged)
	}
	chain, err := pinManagedDirectoryParent(ctx, root, parent, api)
	if err != nil {
		return nil, err
	}
	fail := func(operationErr error) (*DirectoryLease, error) {
		return nil, errors.Join(operationErr, chain.close())
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	leaf := filepath.Base(target.String())
	handle, err := api.ntCreateRelative(
		chain.objects[len(chain.objects)-1].handle,
		leaf,
		managedDirectoryCreateSpec(),
	)
	if err != nil {
		return fail(&FileError{
			Operation: "create-relative-directory",
			Path:      target.String(),
			Err:       err,
		})
	}
	identity, err := api.identity(handle)
	if err != nil {
		return fail(errors.Join(
			&FileError{Operation: "identify-created-directory", Path: target.String(), Err: err},
			closeHandleWithRetry(handle, target.String(), api),
		))
	}
	cleanupCreated := func(operationErr error) (*DirectoryLease, error) {
		return fail(errors.Join(
			operationErr,
			closeHandleWithRetry(handle, target.String(), api),
			deleteCreatedDirectory(chain.objects[len(chain.objects)-1], leaf, target, identity, api),
		))
	}
	if err := ctx.Err(); err != nil {
		return cleanupCreated(err)
	}
	if identity.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return cleanupCreated(unsafeReparseError(target.String()))
	}
	if identity.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return cleanupCreated(&FileError{Operation: "type", Path: target.String(), Err: ErrIdentityChanged})
	}
	finalPath, err := api.finalPath(handle)
	if err != nil {
		return cleanupCreated(&FileError{Operation: "final-path", Path: target.String(), Err: err})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, api)
	if err != nil {
		return cleanupCreated(err)
	}
	if !canonical.Equal(target) {
		return cleanupCreated(outsidePathError("prepare-directory-final", target.String(), ErrIdentityChanged))
	}
	child := pinnedObject{path: canonical, handle: handle, identity: identity}
	if err := validateParentIdentityWith(ctx, chain.objects[len(chain.objects)-1], child, api); err != nil {
		return cleanupCreated(err)
	}
	if err := validatePinnedDirectory(ctx, child, api); err != nil {
		return cleanupCreated(err)
	}
	if err := ctx.Err(); err != nil {
		return cleanupCreated(err)
	}
	chain.objects = append(chain.objects, child)
	return newDirectoryLeaseWithIdentity(target.String(), identity, chain.close), nil
}

func pinManagedDirectoryParent(
	ctx context.Context,
	root CanonicalPath,
	parent CanonicalPath,
	api pathAPI,
) (*pinnedChain, error) {
	if root.Equal(parent) {
		handle, err := api.openPath(root.Native(), directoryPinSpec())
		if err != nil {
			return nil, &FileError{Operation: "open-directory-root", Path: root.String(), Err: err}
		}
		identity, err := api.identity(handle)
		if err != nil {
			return nil, errors.Join(
				&FileError{Operation: "identify-directory-root", Path: root.String(), Err: err},
				wrapFileError("close", root.String(), api.closeHandle(handle)),
			)
		}
		object := pinnedObject{path: root, handle: handle, identity: identity}
		if err := validatePinnedDirectory(ctx, object, api); err != nil {
			return nil, errors.Join(err, api.closeHandle(handle))
		}
		return &pinnedChain{api: api, objects: []pinnedObject{object}}, nil
	}
	return openPinnedChainWith(ctx, root, parent, directoryPinSpec(), api)
}

func managedDirectoryCreateSpec() ntCreateSpec {
	return ntCreateSpec{
		desiredAccess:     windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		shareAccess:       windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		createDisposition: ntFileCreate,
		createOptions:     ntFileDirectoryFile | ntFileSynchronousNonalert | ntFileOpenReparsePoint,
	}
}

func managedDirectoryDeleteSpec() openSpec {
	return openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
}

func deleteCreatedDirectory(
	parent pinnedObject,
	name string,
	target CanonicalPath,
	expected objectIdentity,
	api pathAPI,
) error {
	handle, err := api.openRelative(parent.handle, name, managedDirectoryDeleteSpec())
	if err != nil {
		return &FileError{Operation: "open-created-directory-for-delete", Path: target.String(), Err: err}
	}
	closeOpened := func(operationErr error) error {
		return errors.Join(operationErr, closeHandleWithRetry(handle, target.String(), api))
	}
	actual, err := api.identity(handle)
	if err != nil {
		return closeOpened(&FileError{Operation: "identify-created-directory-for-delete", Path: target.String(), Err: err})
	}
	if !matchesDirectoryIdentity(&expected, actual) {
		return closeOpened(&FileError{Operation: "created-directory-identity", Path: target.String(), Err: ErrIdentityChanged})
	}
	child := pinnedObject{path: target, handle: handle, identity: actual}
	if err := validateParentIdentityWith(context.Background(), parent, child, api); err != nil {
		return closeOpened(err)
	}
	if err := validatePinnedDirectory(context.Background(), child, api); err != nil {
		return closeOpened(err)
	}
	if err := deleteByHandleWith(handle, api); err != nil {
		return closeOpened(err)
	}
	return closeHandleWithRetry(handle, target.String(), api)
}

func closeHandleWithRetry(handle windows.Handle, path string, api pathAPI) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	var closeErrors []error
	for range managedDirectoryCloseAttempts {
		if err := api.closeHandle(handle); err != nil {
			closeErrors = append(closeErrors, wrapFileError("close", path, err))
			continue
		}
		return nil
	}
	return errors.Join(closeErrors...)
}
