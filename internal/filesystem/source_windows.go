//go:build windows

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ReadManagedFile 固定完整路径链，以不共享写入的句柄读取至多 limit 字节。
func ReadManagedFile(ctx context.Context, layout *config.Layout, path string, limit int64) ([]byte, error) {
	if ctx == nil || layout == nil || limit < 0 {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api := newProductionPathAPI()
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return nil, err
	}
	root, err := canonicalizeContextWith(ctx, layout.RepoDir(), api)
	if err != nil {
		return nil, err
	}
	if !root.Contains(target) {
		return nil, outsidePathError("read-source", target.String(), ErrIdentityChanged)
	}
	parent, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return nil, err
	}
	spec := managedFilePinSpec()
	spec.access |= windows.FILE_READ_DATA
	spec.share = windows.FILE_SHARE_READ
	chain, err := openPinnedChainWith(ctx, parent, target, spec, api)
	if err != nil {
		return nil, err
	}
	handle := chain.objects[len(chain.objects)-1].handle
	data := make([]byte, 0, min(limit, 64*1024))
	buffer := make([]byte, min(limit, 64*1024))
	for int64(len(data)) < limit {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, chain.close())
		}
		chunk := buffer[:min(int64(len(buffer)), limit-int64(len(data)))]
		n, readErr := api.readFile(handle, chunk)
		if readErr != nil {
			return nil, errors.Join(readErr, chain.close())
		}
		data = append(data, chunk[:n]...)
		if n == 0 {
			break
		}
	}
	if err := errors.Join(ctx.Err(), chain.close()); err != nil {
		return nil, err
	}
	return data, nil
}

// ReplaceManagedFile 固定父目录并拒绝 Reparse，用同目录临时文件原子替换普通源码文件。
func ReplaceManagedFile(ctx context.Context, layout *config.Layout, path string, data []byte) (returnErr error) {
	if ctx == nil || layout == nil {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := Canonicalize(layout.RepoDir())
	if err != nil {
		return err
	}
	target, err := Canonicalize(path)
	if err != nil {
		return err
	}
	if !root.Contains(target) {
		return outsidePathError("replace-source", target.String(), ErrIdentityChanged)
	}
	parent, err := EnsureManagedDirectory(ctx, layout, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	if _, err := InspectManagedFile(ctx, layout, path); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent.Path(), ".runtime-restore-*")
	if err != nil {
		return fmt.Errorf("create source temporary: %w", err)
	}
	name := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	err = temporary.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 父目录租约禁止路径替换；rename 替换叶子本身，不跟随其链接目标。
	if _, err := InspectManagedFile(ctx, layout, path); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace managed source: %w", err)
	}
	return nil
}
