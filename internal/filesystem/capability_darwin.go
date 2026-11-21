//go:build darwin

package filesystem

import (
	"errors"
	"os"
)

// ErrUnsupported 表示当前平台尚无经过验证的文件系统能力。
var ErrUnsupported = errors.New("filesystem capability is unsupported on this platform")

// pathAPI 留给尚未开放的状态、删除、下载和日志能力；目录与状态事务不经此占位接口。
type pathAPI struct{}

// pinnedObject 把已打开的句柄与它的物理身份绑定，供删除与原子移动复核。
type pinnedObject struct {
	file     *os.File
	identity DirectoryIdentity
}
