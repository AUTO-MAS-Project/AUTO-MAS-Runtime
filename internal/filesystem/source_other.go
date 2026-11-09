//go:build !windows

package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ReadManagedFile 在尚未实现固定路径语义的平台失败关闭。
func ReadManagedFile(context.Context, *config.Layout, string, int64) ([]byte, error) {
	return nil, errors.New("managed source read is unsupported")
}

// ReplaceManagedFile 在尚未实现固定路径语义的平台失败关闭。
func ReplaceManagedFile(context.Context, *config.Layout, string, []byte) error {
	return errors.New("managed source replacement is unsupported")
}
