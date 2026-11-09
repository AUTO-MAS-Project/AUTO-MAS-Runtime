//go:build !windows

package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ManagedSourceReader 在尚未实现固定路径语义的平台失败关闭。
type ManagedSourceReader struct{}

// NewManagedSourceReader 在尚未实现固定路径语义的平台失败关闭。
func NewManagedSourceReader(context.Context, *config.Layout) (*ManagedSourceReader, error) {
	return nil, errors.New("managed source read is unsupported")
}

// Read 在尚未实现固定路径语义的平台失败关闭。
func (*ManagedSourceReader) Read(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("managed source read is unsupported")
}

// Close 不持有任何平台资源。
func (*ManagedSourceReader) Close() error { return nil }
