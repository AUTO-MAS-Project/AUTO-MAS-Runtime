package filesystem

import (
	"context"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// DirectoryIdentity 是受管目录叶子的不可变 volume/file-ID token。
//
// 字段保持包内不可变，调用方只能保存并回传由 InspectManagedDirectory 取得的 token。
type DirectoryIdentity struct {
	volumeSerial  uint64
	fileID        [16]byte
	attributes    uint32
	numberOfLinks uint32
	size          int64
}

// Equal 比较两个目录 token 的稳定对象身份。
//
// 其他属性可能因目录内容变化而改变，目录身份只由 volume serial 与 file ID 决定。
func (i *DirectoryIdentity) Equal(other *DirectoryIdentity) bool {
	return i != nil && other != nil &&
		i.volumeSerial == other.volumeSerial &&
		i.fileID == other.fileID
}

// DirectoryLease 固定一个受管目录及其祖先句柄，防止操作期间路径身份被替换。
type DirectoryLease struct {
	mu       sync.Mutex
	path     string
	identity *DirectoryIdentity
	close    func() error
	closed   bool
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

// OpenManagedDirectory 打开并固定一个已经存在的受管目录。
//
// 返回的租约同时持有目标及其祖先句柄，调用方必须在所有基于该目录的
// 路径读取完成后关闭租约。目标不存在、不是普通目录或身份无法证明时
// 返回错误，且不会创建任何目录。
func OpenManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	return pinManagedDirectory(ctx, layout, path)
}

// PinManagedDirectory 是 OpenManagedDirectory 的语义别名，强调租约在读取期间
// 固定目录身份并阻止同用户通过重命名替换目录叶子。
func PinManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (*DirectoryLease, error) {
	return pinManagedDirectory(ctx, layout, path)
}

// Path 返回租约保护的目录路径。
func (l *DirectoryLease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Identity 返回租约创建时固定的目录叶子身份 token。
func (l *DirectoryLease) Identity() *DirectoryIdentity {
	if l == nil || l.identity == nil {
		return nil
	}
	identity := *l.identity
	return &identity
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

func newDirectoryLeaseWithIdentity(
	path string,
	identity DirectoryIdentity,
	closeFunc func() error,
) *DirectoryLease {
	return &DirectoryLease{path: path, identity: &identity, close: closeFunc}
}
