package filesystem

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

// FileSystemName 只读查询路径实际所在卷的文件系统格式，支持卷挂载目录。
func FileSystemName(ctx context.Context, path string) (string, error) {
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
