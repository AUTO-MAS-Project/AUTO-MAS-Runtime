//go:build windows

package filesystem

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func inspectExternalPath(ctx context.Context, path string, wantDirectory bool) (ExternalPathInspection, error) {
	if ctx == nil || path == "" {
		return ExternalPathInspection{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return ExternalPathInspection{}, err
	}
	api := newProductionPathAPI()
	cleaned, err := cleanAbsoluteWindowsPath(path)
	if err != nil {
		return ExternalPathInspection{}, err
	}
	volume := filepath.VolumeName(cleaned)
	target, err := canonicalizeContextWith(ctx, cleaned, api)
	if err != nil {
		return ExternalPathInspection{}, err
	}
	attributes, attributesErr := api.attributes(target.Native())
	if attributesErr != nil {
		if isWindowsNotFound(attributesErr) {
			if err := inspectWindowsReparseAncestors(ctx, cleaned, volume, api); err != nil {
				return ExternalPathInspection{}, errors.Join(ErrExternalPathUnsafe, err)
			}
			if err := rejectMissingReparseChain(ctx, target, api); err != nil {
				return ExternalPathInspection{}, errors.Join(ErrExternalPathUnsafe, err)
			}
			return ExternalPathInspection{}, nil
		}
		return ExternalPathInspection{}, attributesErr
	}
	isDirectory := attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != wantDirectory {
		return ExternalPathInspection{}, ErrExternalPathNotOrdinary
	}
	volume = filepath.VolumeName(target.String())
	if volume == "" {
		return ExternalPathInspection{}, ErrExternalPathUnsafe
	}
	if err := inspectWindowsReparseAncestors(ctx, cleaned, volume, api); err != nil {
		return ExternalPathInspection{}, err
	}
	root, err := canonicalizeContextWith(ctx, filepath.Dir(target.String()), api)
	if err != nil {
		return ExternalPathInspection{}, err
	}
	leaf := managedFilePinSpec()
	if wantDirectory {
		leaf = directoryPinSpec()
	}
	chain, err := openPinnedChainWith(ctx, root, target, leaf, api)
	if err != nil {
		var fileErr *FileError
		if errors.Is(err, ErrIdentityChanged) || errors.As(err, &fileErr) {
			return ExternalPathInspection{}, errors.Join(ErrExternalPathUnsafe, err)
		}
		return ExternalPathInspection{}, err
	}
	if err := chain.close(); err != nil {
		return ExternalPathInspection{}, err
	}
	return ExternalPathInspection{Exists: true}, nil
}

func inspectWindowsReparseAncestors(ctx context.Context, path, volume string, api pathAPI) error {
	current := filepath.Clean(path)
	volumeRoot := filepath.Clean(volume + string(filepath.Separator))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attributes, err := api.attributes(nativeWindowsPath(current))
		if err != nil {
			if isWindowsNotFound(err) {
				parent := filepath.Dir(current)
				if parent == current {
					return nil
				}
				current = parent
				continue
			}
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return ErrExternalPathUnsafe
		}
		if strings.EqualFold(current, volumeRoot) {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func pathContains(ctx context.Context, parent, child string) (bool, error) {
	if ctx == nil || parent == "" || child == "" {
		return false, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	parentInspection, err := inspectExternalPath(ctx, parent, true)
	if err != nil {
		return false, err
	}
	if !parentInspection.Exists {
		return false, windows.ERROR_PATH_NOT_FOUND
	}
	parentPath, err := Canonicalize(parent)
	if err != nil {
		return false, err
	}
	childPath, err := Canonicalize(child)
	if err != nil {
		return false, err
	}
	if childInspection, inspectErr := inspectExternalPath(ctx, child, true); inspectErr != nil {
		return false, inspectErr
	} else if !childInspection.Exists {
		if err := rejectMissingReparseChain(ctx, childPath, newProductionPathAPI()); err != nil {
			return false, errors.Join(ErrExternalPathUnsafe, err)
		}
	}
	return parentPath.Equal(childPath) || parentPath.Contains(childPath), nil
}
