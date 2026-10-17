//go:build !windows

package filesystem

import (
	"context"
	"errors"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ErrUnsupported 表示当前平台尚无经过验证的文件系统能力。
var ErrUnsupported = errors.New("filesystem capability is unsupported on this platform")

// 这些占位类型不持有平台资源；生产构造函数始终拒绝创建能力。
type pathAPI struct{}
type pinnedObject struct{}

// DirectoryInspection 描述受管目录的只读存在性事实。
type DirectoryInspection struct {
	Exists   bool
	Identity *DirectoryIdentity
}

// CanonicalPath 在当前平台尚不可作为物理路径能力使用。
type CanonicalPath struct{}

// Canonicalize 在原生路径验证尚未实现时失败关闭。
func Canonicalize(string) (CanonicalPath, error) { return CanonicalPath{}, ErrUnsupported }

// String 返回空值，不能用于后续路径授权。
func (CanonicalPath) String() string { return "" }

// Native 返回空值，不能用于后续路径授权。
func (CanonicalPath) Native() string { return "" }

// Equal 不把未验证的两个路径判为同一物理对象。
func (CanonicalPath) Equal(CanonicalPath) bool { return false }

// Contains 不把未验证的路径判为已授权范围。
func (CanonicalPath) Contains(CanonicalPath) bool { return false }

// InspectManagedDirectory 不在身份机制缺失时报告安全目录。
func InspectManagedDirectory(context.Context, *config.Layout, string) (DirectoryInspection, error) {
	return DirectoryInspection{}, ErrUnsupported
}

// New 不创建未经验证的删除能力。
func New(context.Context, *config.Layout, Auditor, ...Option) (*Operator, error) {
	return nil, ErrUnsupported
}

// RemoveTree 不在身份机制缺失时删除目录。
func (*Operator) RemoveTree(context.Context, DeleteRequest) (DeleteResult, error) {
	return DeleteResult{}, ErrUnsupported
}

// AtomicRename 不在身份机制缺失时移动目录。
func (*Operator) AtomicRename(context.Context, RenameRequest) (RenameResult, error) {
	return RenameResult{}, ErrUnsupported
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

// NewDownloadFiles 不创建未经验证的下载发布能力。
func NewDownloadFiles(*config.Layout) (*DownloadFiles, error) { return nil, ErrUnsupported }

// Begin 不在身份机制缺失时创建下载文件。
func (*DownloadFiles) Begin(context.Context, string) (*DownloadSession, error) {
	return nil, ErrUnsupported
}

// Write 不在身份机制缺失时写下载文件。
func (*DownloadSession) Write([]byte) (int, error) { return 0, ErrUnsupported }

// Path 返回空值，不能用于后续路径授权。
func (*DownloadSession) Path() string { return "" }

// PartPath 返回空值，不能用于后续路径授权。
func (*DownloadSession) PartPath() string { return "" }

// PublishNoReplace 不在身份机制缺失时发布文件。
func (*DownloadSession) PublishNoReplace(context.Context) (PublishResult, error) {
	return PublishResult{}, ErrUnsupported
}

// Abort 不在身份机制缺失时删除文件。
func (*DownloadSession) Abort(context.Context) (AbortResult, error) {
	return AbortResult{}, ErrUnsupported
}

// NewRuntimeLogFiles 不创建未经验证的日志能力。
func NewRuntimeLogFiles(context.Context, *config.Layout) (*RuntimeLogFiles, error) {
	return nil, ErrUnsupported
}

// OpenAppend 不在身份机制缺失时写日志。
func (*RuntimeLogFiles) OpenAppend(context.Context, string, time.Time) (*RuntimeLogWriter, error) {
	return nil, ErrUnsupported
}

// List 不在身份机制缺失时颁发删除令牌。
func (*RuntimeLogFiles) List(context.Context) ([]RuntimeLogFile, error) { return nil, ErrUnsupported }

// Remove 不在身份机制缺失时删除日志。
func (*RuntimeLogFiles) Remove(context.Context, RuntimeLogFile) (RemoveResult, error) {
	return RemoveResult{}, ErrUnsupported
}

// Close 幂等收口未持有资源的占位能力。
func (*RuntimeLogFiles) Close() error { return nil }

// Write 不在身份机制缺失时写日志。
func (*RuntimeLogWriter) Write([]byte) (int, error) { return 0, ErrUnsupported }

// Path 返回空值，不能用于后续路径授权。
func (*RuntimeLogWriter) Path() string { return "" }

// Close 幂等收口未持有资源的占位能力。
func (*RuntimeLogWriter) Close() error { return nil }
