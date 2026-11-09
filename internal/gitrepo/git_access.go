package gitrepo

import (
	"github.com/go-git/go-billy/v5"
)

// repositoryAccess 将显示路径和调用方已取得的目录授权一起传给 Git。
type repositoryAccess struct {
	path  string
	lease directoryLease
}

type gitFiles struct {
	worktree billy.Filesystem
	metadata billy.Filesystem
	close    func() error
}

func noGitFilesCleanup() error { return nil }
