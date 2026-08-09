//go:build !windows

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func pathContains(ctx context.Context, parent, child string) (bool, error) {
	if ctx == nil || parent == "" || child == "" {
		return false, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	parentInfo, err := inspectExternalPath(ctx, parent, true)
	if err != nil {
		return false, err
	}
	if !parentInfo.Exists {
		return false, os.ErrNotExist
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false, err
	}
	resolvedChild, err := filepath.EvalSymlinks(child)
	if errors.Is(err, os.ErrNotExist) {
		resolvedParentChild, parentErr := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(child)))
		if parentErr != nil {
			return false, parentErr
		}
		resolvedChild = filepath.Join(resolvedParentChild, filepath.Base(filepath.Clean(child)))
	} else if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(resolvedParent, resolvedChild)
	if err != nil || filepath.IsAbs(rel) {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

func inspectExternalPath(ctx context.Context, path string, wantDirectory bool) (ExternalPathInspection, error) {
	if ctx == nil || path == "" {
		return ExternalPathInspection{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return ExternalPathInspection{}, err
	}
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ExternalPathInspection{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := inspectExternalAncestors(filepath.Dir(path)); err != nil {
				return ExternalPathInspection{}, err
			}
			return ExternalPathInspection{}, nil
		}
		return ExternalPathInspection{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ExternalPathInspection{}, ErrExternalPathUnsafe
	}
	if wantDirectory != info.IsDir() || !wantDirectory && !info.Mode().IsRegular() {
		return ExternalPathInspection{}, ErrExternalPathNotOrdinary
	}
	for current := path; ; current = filepath.Dir(current) {
		ancestor, ancestorErr := os.Lstat(current)
		if ancestorErr != nil {
			if errors.Is(ancestorErr, os.ErrNotExist) {
				break
			}
			return ExternalPathInspection{}, ancestorErr
		}
		if ancestor.Mode()&os.ModeSymlink != 0 {
			return ExternalPathInspection{}, ErrExternalPathUnsafe
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return ExternalPathInspection{Exists: true}, nil
}

func inspectExternalAncestors(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				parent := filepath.Dir(current)
				if parent == current {
					return nil
				}
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrExternalPathUnsafe
		}
		if !info.IsDir() {
			return ErrExternalPathNotOrdinary
		}
		return nil
	}
}
