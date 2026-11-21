//go:build darwin

package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ReadManagedFile 从受管仓库中读取有界普通文件，并复核其身份。
func ReadManagedFile(ctx context.Context, layout *config.Layout, path string, limit int64) ([]byte, error) {
	if ctx == nil || layout == nil || limit < 0 {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := Canonicalize(path)
	if err != nil {
		return nil, err
	}
	repo, err := Canonicalize(layout.RepoDir())
	if err != nil {
		return nil, err
	}
	if !repo.Contains(target) {
		return nil, ErrInvalidArgument
	}
	relative, err := filepath.Rel(repo.path, target.path)
	if err != nil {
		return nil, err
	}
	rootPath, rootRelative, err := darwinManagedRelative(ctx, layout, layout.RepoDir())
	if err != nil {
		return nil, err
	}
	root, err := openDarwinAbsoluteDirectory(ctx, rootPath.path)
	if err != nil {
		return nil, err
	}
	parent, openErr := root.openDirectory(ctx, filepath.Join(rootRelative, filepath.Dir(relative)), false)
	closeErr := root.file.Close()
	if openErr != nil || closeErr != nil {
		if parent != nil {
			closeErr = errors.Join(closeErr, parent.file.Close())
		}
		return nil, errors.Join(openErr, closeErr)
	}
	file, identity, openErr := parent.openFile(ctx, filepath.Base(relative), os.O_RDONLY, 0)
	if openErr != nil {
		return nil, errors.Join(openErr, parent.file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit))
	if readErr == nil {
		_, readErr = parent.verifyFile(filepath.Base(relative), file, identity)
	}
	readErr = errors.Join(readErr, ctx.Err(), file.Close(), parent.file.Close())
	if readErr != nil {
		return nil, readErr
	}
	return data, nil
}

// ReplaceManagedFile 在 macOS 全局源码替换能力开放前失败关闭。
func ReplaceManagedFile(context.Context, *config.Layout, string, []byte) error {
	return errors.New("managed source replacement requires a rooted Git context")
}
