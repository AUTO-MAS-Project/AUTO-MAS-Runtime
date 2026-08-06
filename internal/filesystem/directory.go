package filesystem

import (
	"context"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// DirectoryLease 固定一个受管目录及其祖先句柄，防止操作期间路径身份被替换。
type DirectoryLease struct {
	mu     sync.Mutex
	path   string
	close  func() error
	closed bool
}

// PrepareManagedDirectory 在受管根内原子准备一个新的目录租约。
//
// 目标必须不存在；调用方应在所有基于该目录的 I/O 完成后关闭租约。
func PrepareManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	return prepareManagedDirectory(ctx, layout, path)
}

// Path 返回租约保护的目录路径。
func (l *DirectoryLease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close 释放租约持有的所有句柄；重复关闭是幂等的。
func (l *DirectoryLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.close == nil {
		l.closed = true
		return nil
	}
	if err := l.close(); err != nil {
		return err
	}
	l.closed = true
	return nil
}

func newDirectoryLease(path string, closeFunc func() error) *DirectoryLease {
	return &DirectoryLease{path: path, close: closeFunc}
}
