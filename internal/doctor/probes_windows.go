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

// probeFileSystem 只读获取应用目录所在卷的文件系统名称。
func probeFileSystem(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("encode volume path: %w", err)
	}
	root := make([]uint16, windows.MAX_PATH)
	if err := windows.GetVolumePathName(pathPointer, &root[0], uint32(len(root))); err != nil {
		return "", fmt.Errorf("query volume root: %w", err)
	}
	format := make([]uint16, windows.MAX_PATH)
	if err := windows.GetVolumeInformation(&root[0], nil, 0, nil, nil, nil,
		&format[0], uint32(len(format))); err != nil {
		return "", fmt.Errorf("query file system: %w", err)
	}
	return windows.UTF16ToString(format), nil
}
