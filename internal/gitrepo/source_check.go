package gitrepo

import (
	"context"
	"errors"
	"path"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

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
	// 旧提交可没有能力声明；已声明的不能因磁盘文件损坏而跳过业务放行。
	res, err := tree.Tree(path.Dir(config.BackendCapabilitiesPath))
	if err != nil {
		return false, err
	}
	if err := verifySourceObject(ctx, repo, plumbing.TreeObject, res.Hash); err != nil {
		return false, err
	}
	inputs := []string{"main.py", ".python-version", "pyproject.toml", "uv.lock"}
	if _, err := res.FindEntry(path.Base(config.BackendCapabilitiesPath)); !errors.Is(err, object.ErrEntryNotFound) {
		inputs = append(inputs, config.BackendCapabilitiesPath)
	}
	for _, name := range inputs {
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
	app, err := tree.Tree("app")
	if err != nil {
		return false, err
	}
	return s.startupModulesMatch(ctx, repo, app, reader, "app")
}

// startupModulesMatch 从当前提交枚举 Python 包，覆盖包初始化、传递导入和动态导入。
func (s *Service) startupModulesMatch(ctx context.Context, repo *git.Repository, tree *object.Tree, reader *filesystem.ManagedSourceReader, prefix string) (bool, error) {
	if err := verifySourceObject(ctx, repo, plumbing.TreeObject, tree.Hash); err != nil {
		return false, err
	}
	for _, entry := range tree.Entries {
		name := path.Join(prefix, entry.Name)
		if entry.Mode == filemode.Dir {
			child, err := tree.Tree(entry.Name)
			if err != nil {
				return false, err
			}
			if ok, err := s.startupModulesMatch(ctx, repo, child, reader, name); !ok || err != nil {
				return false, err
			}
			continue
		}
		if !strings.EqualFold(path.Ext(name), ".py") {
			continue
		}
		file, err := tree.TreeEntryFile(&entry)
		if err != nil {
			return false, err
		}
		target, err := s.layout.RepositoryFile(name)
		if err != nil {
			return false, err
		}
		if ok, err := trackedSourceMatches(ctx, reader, file, target); !ok || err != nil {
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
	count, err := s.reader.Restore(ctx, s.layout, commit, nil)
	if err == nil && count == 0 {
		_, err = files.RemoveTransactionIfUnchanged(ctx, mark)
	}
	return count != 0, err
}
