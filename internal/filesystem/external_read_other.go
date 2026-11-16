//go:build !windows

package filesystem

import (
	"context"
	"errors"
)

// ReadExternalFile 在未支持固定路径句柄的平台失败关闭。
func ReadExternalFile(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("external file read is unsupported")
}
