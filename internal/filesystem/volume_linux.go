package filesystem

import (
	"context"

	"golang.org/x/sys/unix"
)

// FileSystemName 返回内核确认的本机文件系统格式。
func FileSystemName(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return "", err
	}
	switch info.Type {
	case unix.EXT4_SUPER_MAGIC:
		return "ext4", nil
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs", nil
	case unix.XFS_SUPER_MAGIC:
		return "xfs", nil
	case unix.TMPFS_MAGIC:
		return "tmpfs", nil
	case unix.OVERLAYFS_SUPER_MAGIC:
		return "overlayfs", nil
	}
	return "unknown", nil
}
