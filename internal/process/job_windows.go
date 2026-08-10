//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
		if err == nil {
			err = errors.New("set information job object returned zero")
		}
		return nil, errors.Join(err, windows.CloseHandle(handle))
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
	assignErr := windows.AssignProcessToJobObject(j.handle, processHandle)
	closeErr := windows.CloseHandle(processHandle)
	return errors.Join(assignErr, closeErr)
}

func (j *windowsJob) assignProcess(process *os.Process) error {
	if j == nil || process == nil {
		return errors.New("process job assignment is invalid")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("process job is closed")
	}
	var assignErr error
	if err := process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(j.handle, windows.Handle(handle))
	}); err != nil {
		return err
	}
	return assignErr
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
	j.closed = true
	return j.closeErr
}

func (j *windowsJob) snapshot() ([]Info, error) {
	if j == nil {
		return nil, errors.New("process job is nil")
	}
	pids, err := j.processIDs()
	if err != nil || len(pids) == 0 {
		return nil, err
	}
	entries, err := processEntries()
	if err != nil {
		return nil, err
	}
	result := make([]Info, 0, len(pids))
	for _, pid := range pids {
		entry, ok := entries[pid]
		if !ok {
			return nil, fmt.Errorf("query process job member %d identity: process entry is missing", pid)
		}
		path, pathErr := processImagePath(pid)
		if pathErr != nil {
			return nil, fmt.Errorf("query process job member %d image: %w", pid, pathErr)
		}
		if path == "" {
			return nil, fmt.Errorf("query process job member %d image: path is empty", pid)
		}
		info := Info{PID: pid, ParentPID: entry.ParentProcessID, Executable: filepath.Clean(path)}
		result = append(result, info)
	}
	return result, nil
}

func (j *windowsJob) processIDs() ([]uint32, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, errors.New("process job is closed")
	}
	capacity := uint32(16)
	for attempts := 0; attempts < 8; attempts++ {
		buffer := make([]byte, 8+int(capacity)*int(unsafe.Sizeof(uintptr(0))))
		err := windows.QueryInformationJobObject(
			j.handle,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buffer[0])),
			uint32(len(buffer)),
			nil,
		)
		assigned := *(*uint32)(unsafe.Pointer(&buffer[0]))
		count := *(*uint32)(unsafe.Pointer(&buffer[4]))
		if errors.Is(err, windows.ERROR_MORE_DATA) || assigned > capacity {
			capacity = assigned
			if capacity == 0 {
				capacity = count + 1
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if count > capacity {
			return nil, errors.New("query process job returned an invalid count")
		}
		result := make([]uint32, 0, count)
		stride := int(unsafe.Sizeof(uintptr(0)))
		for index := uint32(0); index < count; index++ {
			offset := 8 + int(index)*stride
			pid := *(*uintptr)(unsafe.Pointer(&buffer[offset]))
			if pid != 0 {
				result = append(result, uint32(pid))
			}
		}
		return result, nil
	}
	return nil, errors.New("query process job membership did not stabilize")
}

func (j *windowsJob) waitEmpty(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := j.processIDs()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for process job to become empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func processEntries() (map[uint32]windows.ProcessEntry32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	entries := make(map[uint32]windows.ProcessEntry32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		entries[entry.ProcessID] = entry
		entry.Size = uint32(unsafe.Sizeof(entry))
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, errors.Join(err, windows.CloseHandle(snapshot))
	}
	if err := windows.CloseHandle(snapshot); err != nil {
		return nil, err
	}
	return entries, nil
}

func processImagePath(pid uint32) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	queryErr := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size)
	closeErr := windows.CloseHandle(handle)
	if queryErr != nil || closeErr != nil {
		return "", errors.Join(queryErr, closeErr)
	}
	return windows.UTF16ToString(buffer[:size]), nil
}
