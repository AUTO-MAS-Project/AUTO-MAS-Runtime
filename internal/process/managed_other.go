//go:build !windows

package process

import "context"

// StartManaged 在非 Windows 平台失败关闭，绝不降级为普通子进程。
func StartManaged(context.Context, StartSpec) (*ManagedProcess, error) {
	return nil, ErrUnsupported
}
