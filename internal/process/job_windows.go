//go:build windows

package process

import (
	"errors"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsJob struct {
	mu       sync.Mutex
	handle   windows.Handle
	closed   bool
	closeErr error
}

// Supported 报告 Windows 已提供 Job Object 进程树回收实现。
func Supported() bool { return true }

// NewJob 创建带 KILL_ON_JOB_CLOSE 的 Windows Job Object。
func NewJob() (Job, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if result, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil || result == 0 {
		_ = windows.CloseHandle(handle)
		if err == nil {
			err = errors.New("SetInformationJobObject returned zero")
		}
		return nil, err
	}
	return &windowsJob{handle: handle}, nil
}

// Assign 把指定 PID 的进程加入 Job Object。
func (j *windowsJob) Assign(pid uint32) error {
	if j == nil || pid == 0 {
		return errors.New("process job assignment is invalid")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("process job is closed")
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		pid,
	)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(processHandle) }()
	return windows.AssignProcessToJobObject(j.handle, processHandle)
}

// Terminate 终止 Job Object 中的全部进程。
func (j *windowsJob) Terminate(exitCode uint32) error {
	if j == nil {
		return errors.New("process job is nil")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	return windows.TerminateJobObject(j.handle, exitCode)
}

// Close 关闭 Job Object，触发 KILL_ON_JOB_CLOSE 回收剩余进程。
func (j *windowsJob) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return j.closeErr
	}
	j.closeErr = windows.CloseHandle(j.handle)
	if j.closeErr == nil {
		j.closed = true
	}
	return j.closeErr
}
