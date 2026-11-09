//go:build !darwin

package gitrepo

import (
	"context"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
)

func openGitFiles(_ context.Context, access repositoryAccess) (gitFiles, error) {
	worktree := osfs.New(access.path)
	metadata, err := worktree.Chroot(git.GitDirName)
	if err != nil {
		return gitFiles{}, err
	}
	return gitFiles{worktree: worktree, metadata: metadata, close: noGitFilesCleanup}, nil
}

func openGitRepository(_ context.Context, access repositoryAccess) (*git.Repository, func() error, error) {
	repo, err := git.PlainOpen(access.path)
	return repo, noGitFilesCleanup, err
}
