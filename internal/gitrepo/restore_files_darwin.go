package gitrepo

import (
	"context"
	"path/filepath"

	git "github.com/go-git/go-git/v5"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func restoreFilesFor(repo *git.Repository, layout *config.Layout) (sourceRestoreFiles, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return sourceRestoreFiles{}, err
	}
	volume, ok := worktree.Filesystem.(*darwinGitFS)
	if !ok {
		return sourceRestoreFiles{}, errDirectoryIdentityMissing
	}
	return sourceRestoreFiles{
		read: func(ctx context.Context, target string, limit int64) (snapshot sourceFileSnapshot, returnErr error) {
			name, err := filepath.Rel(layout.RepoDir(), target)
			if err != nil {
				return snapshot, err
			}
			returnErr = volume.withPath(name, func(ctx context.Context, full string) error {
				data, identity, err := volume.owner.root.ReadSource(ctx, full, limit)
				snapshot = sourceFileSnapshot{data: data, identity: identity, exists: identity != nil}
				return err
			})
			return snapshot, returnErr
		},
		replace: func(ctx context.Context, target sourceRestore) (result filesystem.PublishResult, returnErr error) {
			name, err := filepath.Rel(layout.RepoDir(), target.path)
			if err != nil {
				return result, err
			}
			returnErr = volume.withPath(name, func(ctx context.Context, full string) error {
				var err error
				result, err = volume.owner.root.ReplaceSource(ctx, full, target.payload, target.perm, target.expected)
				return err
			})
			return result, returnErr
		},
	}, nil
}
