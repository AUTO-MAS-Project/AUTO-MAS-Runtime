//go:build !windows

package process

// Supported 报告非 Windows 平台尚未提供 Job Object 等价实现。
func Supported() bool { return false }

// NewJob 在非 Windows 平台等待对应平台的进程组实现。
func NewJob() (Job, error) {
	return nil, ErrUnsupported
}
