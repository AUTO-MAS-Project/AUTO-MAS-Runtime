//go:build !windows

package process

import (
	"context"
	"os"
)

// StartManaged 在非 Windows 平台失败关闭，绝不降级为普通子进程。
func StartManaged(context.Context, StartSpec) (*ManagedProcess, error) {
	return nil, ErrUnsupported
}

// ResumeSuspended 保留挂起恢复的平台边界，不允许无监督降级。
func ResumeSuspended(*os.Process) error { return ErrUnsupported }
