package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func prepareManagedDirectory(ctx context.Context, layout *config.Layout, path string) (*DirectoryLease, error) {
	return leaseDarwinDirectory(ctx, layout, path, true)
}

func pinManagedDirectory(ctx context.Context, layout *config.Layout, path string) (*DirectoryLease, error) {
	return leaseDarwinDirectory(ctx, layout, path, false)
}

func leaseDarwinDirectory(ctx context.Context, layout *config.Layout, path string, create bool) (*DirectoryLease, error) {
	rootPath, relative, err := darwinManagedRelative(ctx, layout, path)
	if err != nil {
		return nil, err
	}
	if create && relative == "." {
		return nil, ErrInvalidArgument
	}
	root, err := openDarwinAbsoluteDirectory(ctx, rootPath.path)
	if err != nil {
		return nil, err
	}
	directory, openErr := root.openDirectory(ctx, relative, create)
	closeErr := root.file.Close()
	if openErr != nil || closeErr != nil {
		if directory != nil {
			closeErr = errors.Join(closeErr, directory.file.Close())
		}
		return nil, joinOperationCleanup(openErr, closeErr)
	}
	lease := newDirectoryLeaseWithIdentity(directory.path, directory.identity, directory.file.Close)
	lease.rootFile = directory.file
	return lease, nil
}

// InspectManagedDirectory 检查所有已存在祖先，不创建缺失目录。
func InspectManagedDirectory(ctx context.Context, layout *config.Layout, path string) (DirectoryInspection, error) {
	lease, err := pinManagedDirectory(ctx, layout, path)
	if isDarwinStableMissing(err) {
		return DirectoryInspection{}, nil
	}
	if err != nil {
		return DirectoryInspection{}, err
	}
	identity := lease.Identity()
	if err := lease.Close(); err != nil {
		return DirectoryInspection{}, err
	}
	return DirectoryInspection{Exists: true, Identity: identity}, nil
}
