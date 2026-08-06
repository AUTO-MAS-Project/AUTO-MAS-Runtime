package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type renameAttempt struct {
	sourceChain *pinnedChain
	parentChain *pinnedChain
	source      *pinnedObject
	parent      *pinnedObject
	destination CanonicalPath
}

// AtomicRename 按受控用途执行 no-replace 的句柄相对原子重命名。
func (o *Operator) AtomicRename(
	ctx context.Context,
	request RenameRequest,
) (RenameResult, error) {
	if err := validateRenameRequestValues(ctx, request); err != nil {
		return RenameResult{}, err
	}
	for attemptIndex := 0; ; attemptIndex++ {
		attempt, err := o.pinRenameAttempt(ctx, request)
		if err != nil {
			if !isTransientRenamePinError(err, request.Source) {
				return RenameResult{}, err
			}
			if attemptIndex >= len(o.delays) {
				return RenameResult{}, occupiedRenameError(request.Destination, err)
			}
			if err := ctx.Err(); err != nil {
				return RenameResult{}, err
			}
			if err := o.wait(ctx, o.delays[attemptIndex]); err != nil {
				return RenameResult{}, err
			}
			continue
		}
		err = renameByHandleWith(
			attempt.source.handle,
			attempt.parent.handle,
			filepath.Base(attempt.destination.String()),
			false,
			o.api,
		)
		if err == nil {
			result := RenameResult{MutationApplied: true}
			closeErr := attempt.close()
			return result, closeErr
		}
		closeErr := attempt.close()
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
			errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return RenameResult{}, errors.Join(
				occupiedRenameError(
					attempt.destination.String(),
					errors.Join(ErrDestinationExists, err),
				),
				closeErr,
			)
		}
		if errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) {
			return RenameResult{}, errors.Join(
				outsidePathError(
					"rename",
					attempt.destination.String(),
					err,
				),
				closeErr,
			)
		}
		if closeErr != nil {
			return RenameResult{}, errors.Join(err, closeErr)
		}
		if !isTransientRenameError(err) || attemptIndex >= len(o.delays) {
			if isTransientRenameError(err) {
				return RenameResult{}, occupiedRenameError(attempt.destination.String(), err)
			}
			return RenameResult{}, &FileError{
				Operation: "rename",
				Path:      attempt.destination.String(),
				Err:       err,
			}
		}
		if err := ctx.Err(); err != nil {
			return RenameResult{}, err
		}
		if err := o.wait(ctx, o.delays[attemptIndex]); err != nil {
			return RenameResult{}, err
		}
	}
}

func validateRenameRequestValues(
	ctx context.Context,
	request RenameRequest,
) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !request.Kind.Valid() || request.Source == "" || request.Destination == "" ||
		validateAuditValue(request.OperationID) != nil ||
		validateAuditValue(request.Reason) != nil {
		return ErrInvalidArgument
	}
	if request.Kind == RenameUVStagingToVersion {
		if request.Version == "" {
			return ErrInvalidArgument
		}
	} else if request.Version != "" {
		return ErrInvalidArgument
	}
	return nil
}

func (o *Operator) pinRenameAttempt(
	ctx context.Context,
	request RenameRequest,
) (*renameAttempt, error) {
	expectedSource, expectedDestination, err := o.expectedRenamePaths(request)
	if err != nil {
		return nil, err
	}
	cleanSource, err := cleanAbsoluteWindowsPath(request.Source)
	if err != nil {
		return nil, err
	}
	cleanDestination, err := cleanAbsoluteWindowsPath(request.Destination)
	if err != nil {
		return nil, err
	}
	volumeResult, err := compareStringOrdinal(
		filepath.VolumeName(cleanSource),
		filepath.VolumeName(cleanDestination),
		true,
	)
	if err != nil || volumeResult != cstrEqual {
		return nil, outsidePathError(
			"authorize-rename",
			cleanDestination,
			ErrIdentityChanged,
		)
	}
	source, err := canonicalizeContextWith(ctx, request.Source, o.api)
	if err != nil {
		return nil, err
	}
	destination, err := canonicalizeContextWith(ctx, request.Destination, o.api)
	if err != nil {
		return nil, err
	}
	expectedSourceCanonical, err := canonicalizeContextWith(ctx, expectedSource, o.api)
	if err != nil {
		return nil, err
	}
	expectedDestinationCanonical, err := canonicalizeContextWith(ctx, expectedDestination, o.api)
	if err != nil {
		return nil, err
	}
	if !source.Equal(expectedSourceCanonical) ||
		!destination.Equal(expectedDestinationCanonical) {
		return nil, outsidePathError("authorize-rename", destination.String(), ErrIdentityChanged)
	}
	volumeResult, err = compareStringOrdinal(source.volumeKey, destination.volumeKey, true)
	if err != nil || volumeResult != cstrEqual {
		return nil, outsidePathError("authorize-rename", destination.String(), ErrIdentityChanged)
	}
	if _, err := o.api.attributes(destination.Native()); err == nil {
		return nil, occupiedRenameError(destination.String(), ErrDestinationExists)
	} else if !isWindowsNotFound(err) {
		return nil, &FileError{Operation: "attributes", Path: destination.String(), Err: err}
	} else if err := rejectMissingReparseChain(ctx, destination, o.api); err != nil {
		return nil, err
	}

	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(o.layout.AppRoot()), o.api)
	if err != nil {
		return nil, err
	}
	sourceChain, err := openPinnedChainWith(
		ctx,
		rootParent,
		source,
		renameSourceSpec(),
		o.api,
	)
	if err != nil {
		return nil, err
	}
	parentCanonical, err := canonicalizeContextWith(ctx, filepath.Dir(destination.String()), o.api)
	if err != nil {
		return nil, errors.Join(err, sourceChain.close())
	}
	parentChain, err := openPinnedChainWith(
		ctx,
		rootParent,
		parentCanonical,
		directoryPinSpec(),
		o.api,
	)
	if err != nil {
		return nil, errors.Join(err, sourceChain.close())
	}
	return &renameAttempt{
		sourceChain: sourceChain,
		parentChain: parentChain,
		source:      &sourceChain.objects[len(sourceChain.objects)-1],
		parent:      &parentChain.objects[len(parentChain.objects)-1],
		destination: destination,
	}, nil
}

func (o *Operator) expectedRenamePaths(
	request RenameRequest,
) (string, string, error) {
	switch request.Kind {
	case RenameRepositoryToRetired:
		destination, err := o.layout.RepoPreviousDir(request.OperationID)
		return o.layout.RepoDir(), destination, wrapLayoutArgument(err)
	case RenameUpdateToRepository:
		source, err := o.layout.RepoUpdateDir(request.OperationID)
		return source, o.layout.RepoDir(), wrapLayoutArgument(err)
	case RenameRepositoryRollback:
		source, err := o.layout.RepoPreviousDir(request.OperationID)
		return source, o.layout.RepoDir(), wrapLayoutArgument(err)
	case RenameUVStagingToVersion:
		source, sourceErr := o.layout.UVStagingDir(request.Version, request.OperationID)
		destination, destinationErr := o.layout.UVVersionDir(request.Version)
		return source, destination, errors.Join(
			wrapLayoutArgument(sourceErr),
			wrapLayoutArgument(destinationErr),
		)
	default:
		return "", "", ErrInvalidArgument
	}
}

func renameSourceSpec() openSpec {
	return openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
}

func (a *renameAttempt) close() error {
	if a == nil {
		return nil
	}
	return errors.Join(a.parentChain.close(), a.sourceChain.close())
}

func isTransientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func isTransientRenamePinError(err error, source string) bool {
	if err == nil || source == "" {
		return false
	}
	return isTransientRenamePinCause(err, source)
}

func isTransientRenamePinCause(err error, source string) bool {
	if err == nil {
		return false
	}
	if fileErr, ok := err.(*FileError); ok {
		return fileErr.Operation == "open-relative" &&
			sameRenamePath(fileErr.Path, source) &&
			isTransientRenameError(fileErr.Err)
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		matched := false
		for _, child := range multi.Unwrap() {
			if child == nil {
				continue
			}
			if !isTransientRenamePinCause(child, source) {
				return false
			}
			matched = true
		}
		return matched
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return isTransientRenamePinCause(single.Unwrap(), source)
	}
	return false
}

func sameRenamePath(left, right string) bool {
	left = filepath.Clean(strings.ReplaceAll(left, "/", `\`))
	right = filepath.Clean(strings.ReplaceAll(right, "/", `\`))
	return strings.EqualFold(left, right)
}

func occupiedRenameError(path string, cause error) error {
	return &Error{
		code:      protocol.CodeDirectoryOccupied,
		Operation: "rename",
		Path:      path,
		Err:       cause,
	}
}
