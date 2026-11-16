//go:build darwin

package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ReadManagedFile 在 macOS 全局源码读取能力开放前失败关闭。
func ReadManagedFile(context.Context, *config.Layout, string, int64) ([]byte, error) {
	return nil, errors.New("managed source read requires a rooted Git context")
}

// ReplaceManagedFile 在 macOS 全局源码替换能力开放前失败关闭。
func ReplaceManagedFile(context.Context, *config.Layout, string, []byte) error {
	return errors.New("managed source replacement requires a rooted Git context")
}
