//go:build linux || darwin

package doctor

import (
	"context"

	"golang.org/x/sys/unix"
)

func probeDiskFree(ctx context.Context, path string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return 0, err
	}
	return uint64(info.Bavail) * uint64(info.Bsize), nil
}
