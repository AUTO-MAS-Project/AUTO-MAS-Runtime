package gitrepo

import (
	"context"
	"errors"
	"io/fs"
	"path"

	git "github.com/go-git/go-git/v5"
	gitcache "github.com/go-git/go-git/v5/plumbing/cache"
	gitfilesystem "github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func openGitFiles(ctx context.Context, access repositoryAccess) (gitFiles, error) {
	lease, ok := access.lease.(*filesystem.DirectoryLease)
	if !ok || lease == nil {
		return gitFiles{}, errDirectoryIdentityMissing
	}
	expected, err := filesystem.Canonicalize(access.path)
	if err != nil {
		return gitFiles{}, err
	}
	actual, err := filesystem.Canonicalize(lease.Path())
	if err != nil || !expected.Equal(actual) {
		return gitFiles{}, errors.Join(filesystem.ErrIdentityChanged, err)
	}
	root, err := lease.RootedDirectory()
	if err != nil {
		return gitFiles{}, err
	}
	if err := root.CheckLocation(ctx); err != nil {
		return gitFiles{}, err
	}
	worktree := newDarwinGitFS(ctx, root, lease.Path())
	metadata := &darwinGitFS{owner: worktree.owner, prefix: git.GitDirName}
	return gitFiles{worktree: worktree, metadata: metadata, close: func() error {
		return errors.Join(worktree.owner.close(), root.CheckLocation(ctx))
	}}, nil
}

func openGitRepository(ctx context.Context, access repositoryAccess) (*git.Repository, func() error, error) {
	files, err := openGitFiles(ctx, access)
	if err != nil {
		return nil, nil, err
	}
	metadata, err := files.metadata.Stat(".")
	if err != nil || !metadata.IsDir() {
		return nil, nil, errors.Join(git.ErrRepositoryNotExists, err, files.close())
	}
	// Runtime 只接受自包含的工作树，避免 Git 使用其他仓库的配置或对象。
	for _, special := range []string{"commondir", path.Join("objects", "info", "alternates"), path.Join("objects", "info", "http-alternates")} {
		if _, err := files.metadata.Stat(special); !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, errors.Join(errInvalidRepositoryID, err, files.close())
		}
	}
	repo, err := git.Open(gitfilesystem.NewStorage(files.metadata, gitcache.NewObjectLRUDefault()), files.worktree)
	if err != nil {
		return nil, nil, errors.Join(err, files.close())
	}
	return repo, files.close, nil
}
