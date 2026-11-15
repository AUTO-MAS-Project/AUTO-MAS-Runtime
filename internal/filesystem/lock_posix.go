//go:build linux || darwin

package filesystem

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// OpenManagedDirectoryHandle 固定目录 inode，供内核门闩和同对象元数据操作使用。
func OpenManagedDirectoryHandle(ctx context.Context, layout *config.Layout, path string) (*os.File, error) {
	c, err := managedPOSIX(ctx, layout, path, false)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(c.leaf().Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	err = errors.Join(err, c.verify(ctx), c.close())
	if err != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
