package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func prepareManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	if ctx == nil || layout == nil || path == "" {
		return nil, fmt.Errorf("%w: invalid managed-directory lease input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api := newProductionPathAPI()
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
			wrapFileError("close", target.String(), api.closeHandle(handle)),
		))
	}
	closeCreated := func(operationErr error) (*DirectoryLease, error) {
		return fail(errors.Join(
			operationErr,
			wrapFileError("close", target.String(), api.closeHandle(handle)),
		))
	}
	if identity.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return closeCreated(unsafeReparseError(target.String()))
	}
	if identity.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return closeCreated(&FileError{Operation: "type", Path: target.String(), Err: ErrIdentityChanged})
	}
	finalPath, err := api.finalPath(handle)
	if err != nil {
		return closeCreated(&FileError{Operation: "final-path", Path: target.String(), Err: err})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, api)
	if err != nil {
		return closeCreated(err)
	}
	if !canonical.Equal(target) {
		return closeCreated(outsidePathError("prepare-directory-final", target.String(), ErrIdentityChanged))
	}
	child := pinnedObject{path: canonical, handle: handle, identity: identity}
	if err := validateParentIdentityWith(ctx, chain.objects[len(chain.objects)-1], child, api); err != nil {
		return closeCreated(err)
	}
	if err := validatePinnedDirectory(ctx, child, api); err != nil {
		return closeCreated(err)
	}
	chain.objects = append(chain.objects, child)
	return newDirectoryLease(target.String(), chain.close), nil
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
