package filesystem

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

// FileSystemName 只读查询路径所在卷的文件系统格式。
func FileSystemName(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", fmt.Errorf("query file system: %w", err)
	}
	return unix.ByteSliceToString(stat.Fstypename[:]), nil
}
