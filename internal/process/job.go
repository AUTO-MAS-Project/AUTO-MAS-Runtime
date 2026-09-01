package process

import "errors"

// ErrUnsupported 表示当前平台没有 Job Object 等价实现。
var ErrUnsupported = errors.New("process job is unsupported")

// Job 负责把 Runtime 创建的进程及其子进程绑定到同一回收边界；
// 只有显式带 CREATE_BREAKAWAY_FROM_JOB 创建的进程在该边界之外。
type Job interface {
	Assign(uint32) error
	Terminate(uint32) error
	Close() error
}
