//go:build windows

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func inspectManagedFile(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (FileInspection, error) {
	if ctx == nil || layout == nil || path == "" {
		return FileInspection{}, fmt.Errorf("%w: invalid managed-file input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return FileInspection{}, err
	}
	api := newProductionPathAPI()
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return FileInspection{}, err
	}
	attributes, attributesErr := api.attributes(target.Native())
	if attributesErr != nil {
		if !isWindowsNotFound(attributesErr) {
			return FileInspection{}, &FileError{Operation: "attributes", Path: target.String(), Err: attributesErr}
		}
		if err := validateExistingLayoutPathWith(ctx, layout, path, api); err != nil {
			return FileInspection{}, err
		}
		if err := rejectMissingReparseChain(ctx, target, api); err != nil {
			return FileInspection{}, err
		}
		return FileInspection{Exists: false}, nil
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return FileInspection{}, &FileError{Operation: "type", Path: target.String(), Err: ErrIdentityChanged}
	}
	root, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return FileInspection{}, err
	}
	if !root.Contains(target) {
		return FileInspection{}, outsidePathError("inspect-managed-file-root", target.String(), ErrIdentityChanged)
	}
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(root.String()), api)
	if err != nil {
		return FileInspection{}, err
	}
	chain, err := openPinnedChainWith(ctx, rootParent, target, managedFilePinSpec(), api)
	if err != nil {
		return FileInspection{}, err
	}
	if err := ctx.Err(); err != nil {
		return FileInspection{}, errors.Join(err, chain.close())
	}
	if err := chain.close(); err != nil {
		return FileInspection{}, err
	}
	return FileInspection{Exists: true}, nil
}

func managedFilePinSpec() openSpec {
	return openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
}
