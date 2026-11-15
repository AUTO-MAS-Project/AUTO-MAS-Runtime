//go:build linux || darwin

package process

// Supported 报告本机具备带守护进程的原生监督实现。
func Supported() bool { return true }

// NewJob 在 POSIX 上拒绝先启动再赋组的非原子路径；使用 StartManaged 在执行前建立 session。
func NewJob() (Job, error) { return nil, ErrUnsupported }
