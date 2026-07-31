//go:build windows

package state

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

var errUnexpectedProcessWait = errors.New("unexpected process wait result")

type processAPI struct {
	openProcess func(
		desiredAccess uint32,
		inheritHandle bool,
		processID uint32,
	) (windows.Handle, error)
	waitForSingleObject func(
		handle windows.Handle,
		milliseconds uint32,
	) (uint32, error)
	closeHandle func(handle windows.Handle) error
}

// SystemPIDProbe 通过 SYNCHRONIZE handle 查询一个指定 PID；零值可用。
type SystemPIDProbe struct {
	api *processAPI
}

// NewSystemPIDProbe 创建使用真实 Win32 API 的 PID 探针。
func NewSystemPIDProbe() *SystemPIDProbe {
	return &SystemPIDProbe{}
}

func newSystemPIDProbeWith(api processAPI) *SystemPIDProbe {
	copyOfAPI := api
	return &SystemPIDProbe{api: &copyOfAPI}
}

// Alive 以零等待判断指定 PID 对应 process object 是否仍未 signaled。
func (p *SystemPIDProbe) Alive(
	ctx context.Context,
	pid uint32,
) (bool, error) {
	if ctx == nil {
		return false, validationError("ctx")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pid == 0 {
		return false, ErrInvalidPID
	}
	api := p.processAPI()
	if api.openProcess == nil ||
		api.waitForSingleObject == nil ||
		api.closeHandle == nil {
		return false, &PIDProbeError{
			Operation: "open-process",
			PID:       pid,
			Cause:     errInvalidValue,
		}
	}

	handle, err := api.openProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
			errors.Is(err, windows.ERROR_NOT_FOUND) {
			return false, nil
		}
		return false, &PIDProbeError{
			Operation: "open-process",
			PID:       pid,
			Cause:     err,
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		var closeErr error
		if err := api.closeHandle(handle); err != nil {
			closeErr = &PIDProbeError{
				Operation: "close-handle",
				PID:       pid,
				Cause:     err,
			}
		}
		return false, joinProbeErrors(ctxErr, closeErr)
	}

	waitResult, waitCause := api.waitForSingleObject(handle, 0)
	alive := false
	var waitErr error
	if waitCause != nil {
		waitErr = &PIDProbeError{
			Operation: "wait-process",
			PID:       pid,
			Cause:     waitCause,
		}
	} else {
		switch waitResult {
		case uint32(windows.WAIT_TIMEOUT):
			alive = true
		case windows.WAIT_OBJECT_0:
			alive = false
		default:
			waitErr = &PIDProbeError{
				Operation: "wait-process",
				PID:       pid,
				Cause:     errUnexpectedProcessWait,
			}
		}
	}

	var closeErr error
	if err := api.closeHandle(handle); err != nil {
		closeErr = &PIDProbeError{
			Operation: "close-handle",
			PID:       pid,
			Cause:     err,
		}
	}
	ctxErr := ctx.Err()
	if err := joinProbeErrors(waitErr, closeErr, ctxErr); err != nil {
		return false, err
	}
	return alive, nil
}

func (p *SystemPIDProbe) processAPI() processAPI {
	if p != nil && p.api != nil {
		return *p.api
	}
	return processAPI{
		openProcess:         windows.OpenProcess,
		waitForSingleObject: windows.WaitForSingleObject,
		closeHandle:         windows.CloseHandle,
	}
}

func joinProbeErrors(values ...error) error {
	var result error
	for _, value := range values {
		if value == nil {
			continue
		}
		if result == nil {
			result = value
			continue
		}
		result = fmt.Errorf("%w: %w", result, value)
	}
	return result
}
