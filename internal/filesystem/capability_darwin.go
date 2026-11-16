//go:build darwin

package filesystem

import (
	"context"
	"errors"
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ErrUnsupported 表示当前平台尚无经过验证的文件系统能力。
var ErrUnsupported = errors.New("filesystem capability is unsupported on this platform")

// pathAPI 留给尚未开放的状态、删除、下载和日志能力；目录能力不经此占位接口。
type pathAPI struct{}
type pinnedObject struct {
	file     *os.File
	identity DirectoryIdentity
}

// NewStateFiles 不创建未经验证的事务能力。
func NewStateFiles(context.Context, *config.Layout) (*StateFiles, error) {
	return nil, ErrUnsupported
}

// Read 不在身份机制缺失时读取状态。
func (*StateFiles) Read(context.Context, StateFileKind, int64) (StateFileSnapshot, error) {
	return StateFileSnapshot{}, ErrUnsupported
}

// WriteAtomic 不在事务机制缺失时写状态。
func (*StateFiles) WriteAtomic(context.Context, StateFileKind, []byte) (WriteAtomicResult, error) {
	return WriteAtomicResult{}, ErrUnsupported
}

// RemoveTransactionIfUnchanged 不在条件删除机制缺失时移除状态。
func (*StateFiles) RemoveTransactionIfUnchanged(context.Context, StateFileSnapshot) (StateRemoveResult, error) {
	return StateRemoveResult{}, ErrUnsupported
}

// Close 幂等收口未持有资源的占位能力。
func (*StateFiles) Close() error { return nil }
