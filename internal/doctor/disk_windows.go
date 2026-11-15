package doctor

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

// probeDiskFree 查询路径所在卷的可用字节数。
func probeDiskFree(ctx context.Context, path string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode disk path: %w", err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(
		pathPointer,
		&freeBytesAvailable,
		&totalBytes,
		&totalFreeBytes,
	); err != nil {
		return 0, fmt.Errorf("query disk free space: %w", err)
	}
	return freeBytesAvailable, nil
}
