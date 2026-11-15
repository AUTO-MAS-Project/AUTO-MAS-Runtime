package filesystem

import (
	"context"
	"strings"

	"golang.org/x/sys/unix"
)

// FileSystemName 返回内核报告的实际挂载格式。
func FileSystemName(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return "", err
	}
	name := strings.TrimRight(string(info.Fstypename[:]), "\x00")
	if name != "apfs" && name != "hfs" {
		name = "unknown"
	}
	return name, nil
}
