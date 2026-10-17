package doctor

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

func probeDiskFree(ctx context.Context, path string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("query disk free space: %w", err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func probeFileSystem(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", fmt.Errorf("query file system: %w", err)
	}
	return unix.ByteSliceToString(stat.Fstypename[:]), nil
}
