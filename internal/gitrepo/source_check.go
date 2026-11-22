package gitrepo

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// deferSourceCheck 在 mutation 锁下登记本轮核对；上次未完成则保留原有前台恢复。
func (s *Service) deferSourceCheck(ctx context.Context, files *filesystem.StateFiles, commit string) (bool, *filesystem.StateFileSnapshot, error) {
	prior, err := files.Read(ctx, filesystem.StateSourceCheck, filesystem.MaxStateFileBytes)
	if err == nil {
		return false, &prior, nil
	}
	if !errors.Is(err, filesystem.ErrStateFileNotFound) {
		return false, nil, err
	}

	// 入口或环境输入损坏会使监督器根本无法启动，仍须在本次离线恢复。
	match, err := s.startupInputsMatch(ctx, commit)
	if err != nil || !match {
		return false, nil, ctx.Err()
	}
	_, err = files.WriteAtomic(ctx, filesystem.StateSourceCheck, []byte(commit))
	return err == nil, nil, err
}

func (s *Service) startupInputsMatch(ctx context.Context, commit string) (match bool, returnErr error) {
	repo, tree, err := sourceTree(ctx, s.layout, commit)
	if err != nil {
		return false, err
	}
	if err := verifySourceObject(ctx, repo, plumbing.TreeObject, tree.Hash); err != nil {
		return false, err
	}
	reader, err := filesystem.NewManagedSourceReader(ctx, s.layout)
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, reader.Close()) }()
	for _, name := range [...]string{"main.py", ".python-version", "pyproject.toml", "uv.lock"} {
		file, err := tree.File(name)
		if err != nil {
			return false, err
		}
		path, err := s.layout.RepositoryFile(name)
		if err != nil {
			return false, err
		}
		if ok, err := trackedSourceMatches(ctx, reader, file, path); !ok || err != nil {
			return false, err
		}
	}
	return true, nil
}

// VerifyPendingSource 在 backend 锁下只读核对。发现差异或未完成时保留标记，不写源码。
func (s *Service) VerifyPendingSource(ctx context.Context, commit string) (dirty bool, returnErr error) {
	files, err := filesystem.NewStateFiles(ctx, s.layout)
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, files.Close()) }()
	mark, err := files.Read(ctx, filesystem.StateSourceCheck, filesystem.MaxStateFileBytes)
	if errors.Is(err, filesystem.ErrStateFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(mark.Bytes()) != commit {
		return false, nil
	}
	lease, err := filesystem.PinManagedDirectory(ctx, s.layout, s.layout.RepoDir())
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Close()) }()
	err = inspectTrackedSources(ctx, s.layout, commit, func(*object.File, string) error {
		dirty = true
		return nil
	})
	if err == nil && !dirty {
		_, err = files.RemoveTransactionIfUnchanged(ctx, mark)
	}
	return dirty, err
}
