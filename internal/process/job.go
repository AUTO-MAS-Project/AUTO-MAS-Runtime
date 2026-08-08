package process

import "errors"

// ErrUnsupported 表示当前平台没有 Job Object 等价实现。
var ErrUnsupported = errors.New("process job is unsupported")

// Job 负责把 Runtime 创建的进程及其子进程绑定到同一回收边界。
type Job interface {
	Assign(uint32) error
	Terminate(uint32) error
	Close() error
}
