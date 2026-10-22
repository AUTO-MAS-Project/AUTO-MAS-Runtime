//go:build !windows

package filesystem

import (
	"context"
	"errors"
)

// FileSystemName 在尚未实现卷探测的平台返回不支持。
func FileSystemName(ctx context.Context, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", errors.ErrUnsupported
}
