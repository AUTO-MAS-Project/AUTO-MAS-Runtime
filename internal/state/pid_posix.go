//go:build linux || darwin

package state

import (
	"context"
	"errors"
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// SystemPIDProbe 提供本机 PID 存活和创建身份探测。
type SystemPIDProbe struct{}

// NewSystemPIDProbe 创建本机进程探针。
func NewSystemPIDProbe() *SystemPIDProbe { return &SystemPIDProbe{} }

// Alive 以内核探测区分进程消失和权限不足。
func (*SystemPIDProbe) Alive(ctx context.Context, pid uint32) (bool, error) {
	if ctx == nil {
		return false, ErrInvalidPID
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pid == 0 {
		return false, ErrInvalidPID
	}
	if uint64(pid) > math.MaxInt32 {
		return false, nil
	}
	err := unix.Kill(int(pid), 0)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, &PIDProbeError{Operation: "kill-probe", PID: pid, Cause: err}
	}
	return true, nil
}

// CreatedAt 返回可重复比较的内核创建身份。
func (*SystemPIDProbe) CreatedAt(ctx context.Context, pid uint32) (time.Time, error) {
	if ctx == nil {
		return time.Time{}, ErrInvalidPID
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if pid == 0 || uint64(pid) > math.MaxInt32 {
		return time.Time{}, ErrInvalidPID
	}
	return posixCreatedAt(pid)
}

// Identity 返回不受墙上时钟调整影响的内核进程身份。
func (*SystemPIDProbe) Identity(ctx context.Context, pid uint32) (string, error) {
	if ctx == nil || pid == 0 || uint64(pid) > math.MaxInt32 {
		return "", ErrInvalidPID
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return posixIdentityToken(pid)
}
