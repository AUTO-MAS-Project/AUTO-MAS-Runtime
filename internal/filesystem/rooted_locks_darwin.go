package filesystem

import (
	"context"
	"errors"
	"os"
)

// OpenChildDirectory 固定同一根下的目录；仅确定缺失时返回 nil 租约和 nil 错误。
func (r *RootedDirectory) OpenChildDirectory(ctx context.Context, name string) (*DirectoryLease, error) {
	if name == "." {
		return nil, ErrInvalidArgument
	}
	var lease *DirectoryLease
	err := r.withRoot(ctx, name, func(root *darwinDirectory) error {
		dir, err := root.openDirectory(ctx, name, false)
		if err != nil {
			return err
		}
		lease = newDirectoryLeaseWithIdentity(dir.path, dir.identity, dir.file.Close)
		lease.rootFile = dir.file
		return nil
	})
	if isDarwinStableMissing(err) {
		return nil, nil
	}
	return lease, err
}

// OpenLockFile 打开或准备持久协作锁；缺失的只读探测不创建叶。
func (r *RootedDirectory) OpenLockFile(ctx context.Context, name string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := r.OpenFile(ctx, name, flags, 0o600)
	if !create && isDarwinStableMissing(err) {
		return nil, nil
	}
	return file, err
}

// VerifyLockFile 复核已打开的锁与当前名称仍指向同一个零长度普通文件。
func (r *RootedDirectory) VerifyLockFile(ctx context.Context, name string, file *os.File) (os.FileInfo, error) {
	if file == nil {
		return nil, ErrInvalidArgument
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	named, err := r.Stat(ctx, name)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errors.Join(ErrIdentityChanged, err)
	}
	if !opened.Mode().IsRegular() || opened.Size() != 0 || !named.Mode().IsRegular() ||
		named.Size() != 0 || !os.SameFile(opened, named) {
		return nil, ErrIdentityChanged
	}
	return opened, nil
}
