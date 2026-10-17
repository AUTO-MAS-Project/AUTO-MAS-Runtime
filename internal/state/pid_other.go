//go:build !windows

package state

import (
	"context"
	"errors"
	"time"
)

var errPIDUnsupported = errors.New("process identity probe is unsupported on this platform")

// SystemPIDProbe 在原生身份协议验证前不把未知 PID 判为死亡。
type SystemPIDProbe struct{}

// NewSystemPIDProbe 创建失败关闭的占位探针。
func NewSystemPIDProbe() *SystemPIDProbe { return &SystemPIDProbe{} }

// Alive 在无法证明进程身份时返回错误，防止误删活动事务。
func (*SystemPIDProbe) Alive(context.Context, uint32) (bool, error) { return false, errPIDUnsupported }

// CreatedAt 在无法证明进程身份时返回错误。
func (*SystemPIDProbe) CreatedAt(context.Context, uint32) (time.Time, error) {
	return time.Time{}, errPIDUnsupported
}
