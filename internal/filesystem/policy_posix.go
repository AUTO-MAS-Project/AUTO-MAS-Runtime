//go:build linux || darwin

package filesystem

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

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
	case DeleteUVVersion:
		path, err := o.layout.UVVersionDir(request.Version)
		return path, true, wrapLayoutArgument(err)
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
	case DeleteDependencySync:
		path, err := o.layout.DependencySyncDir(request.OperationID)
		return path, true, wrapLayoutArgument(err)
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
