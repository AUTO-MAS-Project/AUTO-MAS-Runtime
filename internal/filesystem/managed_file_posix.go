//go:build linux

package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func openPOSIXFile(c *posixChain, name string, flags int) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, ErrInvalidArgument
	}
	fd, err := unix.Openat(int(c.leaf().Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	id, err := posixIdentity(f)
	if err == nil && id.attributes&unix.S_IFMT != unix.S_IFREG {
		err = ErrIdentityChanged
	}
	if err == nil && id.numberOfLinks != 1 {
		err = ErrUnsafeHardLink
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
func inspectManagedFile(ctx context.Context, layout *config.Layout, path string) (FileInspection, error) {
	c, err := managedPOSIX(ctx, layout, filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return FileInspection{}, nil
	}
	if err != nil {
		return FileInspection{}, err
	}
	f, err := openPOSIXFile(c, filepath.Base(path), unix.O_RDONLY)
	closeErr := c.close()
	if errors.Is(err, os.ErrNotExist) {
		return FileInspection{}, closeErr
	}
	if err != nil {
		return FileInspection{}, errors.Join(err, closeErr)
	}
	return FileInspection{Exists: true}, errors.Join(closeErr, f.Close())
}

// ReadManagedFile 从逐段拒绝链接的目录句柄读取有界普通文件。
func ReadManagedFile(ctx context.Context, layout *config.Layout, path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrInvalidArgument
	}
	c, err := managedPOSIX(ctx, layout, filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	f, err := openPOSIXFile(c, filepath.Base(path), unix.O_RDONLY)
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		err = ErrStateFileTooLarge
	}
	err = errors.Join(err, c.verify(ctx), f.Close(), c.close())
	return data, err
}

// ReplaceManagedFile 在同目录同步临时文件后原子替换普通文件。
func ReplaceManagedFile(ctx context.Context, layout *config.Layout, path string, data []byte) error {
	c, err := managedPOSIX(ctx, layout, filepath.Dir(path), true)
	if err != nil {
		return err
	}
	_, err = replacePOSIX(ctx, c, filepath.Base(path), data)
	return errors.Join(err, c.close())
}

// ManagedSourceReader 复用受管源读取边界。
type ManagedSourceReader struct {
	layout *config.Layout
	lease  *DirectoryLease
}

// NewManagedSourceReader 固定仓库目录身份。
func NewManagedSourceReader(ctx context.Context, layout *config.Layout) (*ManagedSourceReader, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	lease, err := OpenManagedDirectory(ctx, layout, layout.RepoDir())
	if err != nil {
		return nil, err
	}
	return &ManagedSourceReader{layout: layout, lease: lease}, nil
}

// Read 只读取原始仓库身份下的普通文件。
func (r *ManagedSourceReader) Read(ctx context.Context, path string, limit int64) ([]byte, error) {
	if r == nil || r.lease == nil {
		return nil, ErrClosed
	}
	inspection, err := InspectManagedDirectory(ctx, r.layout, r.layout.RepoDir())
	if err != nil {
		return nil, err
	}
	if !r.lease.Identity().Equal(inspection.Identity) {
		return nil, ErrIdentityChanged
	}
	if !within(r.layout.RepoDir(), path) {
		return nil, ErrInvalidArgument
	}
	return ReadManagedFile(ctx, r.layout, path, limit)
}

// Close 释放目录身份租约。
func (r *ManagedSourceReader) Close() error {
	if r == nil || r.lease == nil {
		return nil
	}
	err := r.lease.Close()
	r.lease = nil
	return err
}
