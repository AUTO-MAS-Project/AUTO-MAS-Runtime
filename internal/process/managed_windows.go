//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var terminateThreadProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("TerminateThread")

// StartManaged 挂起创建进程，在加入 KILL_ON_JOB_CLOSE Job 后才恢复初始线程。
func StartManaged(ctx context.Context, spec StartSpec) (*ManagedProcess, error) {
	return startManagedWindows(ctx, spec, defaultWindowsStartOperations())
}

type windowsStartOperations struct {
	startProcess  func(string, []string, *os.ProcAttr) (*os.Process, error)
	assign        func(managedJob, *os.Process) error
	initialThread func(uint32) (windows.Handle, error)
	resumeThread  func(windows.Handle) (uint32, error)
	closeHandle   func(windows.Handle) error
}

func defaultWindowsStartOperations() windowsStartOperations {
	return windowsStartOperations{
		startProcess: os.StartProcess,
		assign: func(job managedJob, processValue *os.Process) error {
			return job.assignProcess(processValue)
		},
		initialThread: initialThread,
		resumeThread:  windows.ResumeThread,
		closeHandle:   windows.CloseHandle,
	}
}

func startManagedWindows(
	ctx context.Context,
	spec StartSpec,
	operations windowsStartOperations,
) (*ManagedProcess, error) {
	if err := validateStartSpec(ctx, spec); err != nil {
		return nil, err
	}
	jobValue, err := NewJob()
	if err != nil {
		return nil, fmt.Errorf("create managed process job: %w", err)
	}
	job, ok := jobValue.(managedJob)
	if !ok {
		return nil, errors.Join(
			errors.New("create managed process job: implementation lacks managed operations"),
			jobValue.Close(),
		)
	}

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open managed process stdin: %w", err), job.Close())
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create managed process stdout pipe: %w", err),
			stdin.Close(),
			job.Close(),
		)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create managed process stderr pipe: %w", err),
			stdoutRead.Close(),
			stdoutWrite.Close(),
			stdin.Close(),
			job.Close(),
		)
	}

	arguments := make([]string, 0, len(spec.Args)+1)
	arguments = append(arguments, spec.Executable)
	arguments = append(arguments, spec.Args...)
	processValue, startErr := operations.startProcess(spec.Executable, arguments, &os.ProcAttr{
		Dir:   spec.Dir,
		Env:   spec.Env,
		Files: []*os.File{stdin, stdoutWrite, stderrWrite},
		Sys: &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW,
		},
	})
	// 子端句柄已由 CreateProcess 复制，parent 不再持有。
	childEndCloseErr := errors.Join(stdin.Close(), stdoutWrite.Close(), stderrWrite.Close())
	assigned := false
	cleanupStarted := func(cause error) (*ManagedProcess, error) {
		var retryAssignErr error
		if !assigned {
			retryAssignErr = job.assignProcess(processValue)
			if retryAssignErr == nil {
				assigned = true
			}
		}
		var jobTerminateErr error
		if assigned {
			jobTerminateErr = job.Terminate(96)
		}
		terminateErr := terminateStartedProcess(processValue, 96)
		var killErr error
		if terminateErr != nil {
			killErr = processValue.Kill()
		}
		var threadTerminateErr error
		terminationConfirmed := terminateErr == nil || killErr == nil || assigned && jobTerminateErr == nil
		if !terminationConfirmed && !assigned {
			threadTerminateErr = terminateSuspendedInitialThread(uint32(processValue.Pid), 96)
			terminationConfirmed = threadTerminateErr == nil
		}
		var jobErr error
		if !terminationConfirmed && assigned {
			jobErr = job.Close()
			terminationConfirmed = jobErr == nil
		}
		var exitStateErr error
		if !terminationConfirmed {
			var exited bool
			exited, exitStateErr = startedProcessExited(processValue)
			terminationConfirmed = exitStateErr == nil && exited
		}
		var waitErr error
		if terminationConfirmed {
			_, waitErr = processValue.Wait()
		} else {
			waitErr = errors.New("managed process termination could not be confirmed")
		}
		stdoutErr := stdoutRead.Close()
		stderrErr := stderrRead.Close()
		if jobErr == nil {
			jobErr = job.Close()
		}
		return nil, errors.Join(cause, childEndCloseErr, retryAssignErr, jobTerminateErr, terminateErr, killErr, threadTerminateErr, exitStateErr, waitErr, stdoutErr, stderrErr, jobErr)
	}
	if startErr != nil {
		if processValue != nil {
			return cleanupStarted(fmt.Errorf("start managed process suspended: %w", startErr))
		}
		return nil, errors.Join(
			fmt.Errorf("start managed process suspended: %w", startErr),
			childEndCloseErr,
			stdoutRead.Close(),
			stderrRead.Close(),
			job.Close(),
		)
	}
	if childEndCloseErr != nil {
		return cleanupStarted(fmt.Errorf("close managed process child handles: %w", childEndCloseErr))
	}
	if err := operations.assign(job, processValue); err != nil {
		return cleanupStarted(fmt.Errorf("assign managed process to job: %w", err))
	}
	assigned = true
	if err := ctx.Err(); err != nil {
		return cleanupStarted(err)
	}
	thread, err := operations.initialThread(uint32(processValue.Pid))
	if err != nil {
		var threadCloseErr error
		if thread != 0 {
			threadCloseErr = operations.closeHandle(thread)
		}
		return cleanupStarted(errors.Join(fmt.Errorf("find managed process initial thread: %w", err), threadCloseErr))
	}
	previous, resumeErr := operations.resumeThread(thread)
	threadCloseErr := operations.closeHandle(thread)
	if resumeErr != nil || previous != 1 || threadCloseErr != nil {
		if resumeErr == nil && previous != 1 {
			resumeErr = fmt.Errorf("initial thread suspend count was %d, want 1", previous)
		}
		if resumeErr != nil {
			resumeErr = fmt.Errorf("resume managed process initial thread: %w", resumeErr)
		}
		return cleanupStarted(errors.Join(resumeErr, threadCloseErr))
	}
	return ownManagedProcess(ctx, spec.Sink, processValue, job, stdoutRead, stderrRead), nil
}

func validateStartSpec(ctx context.Context, spec StartSpec) error {
	if ctx == nil {
		return errors.New("managed process context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.Executable == "" || !filepath.IsAbs(spec.Executable) {
		return errors.New("managed process executable must be absolute")
	}
	if spec.Dir == "" || !filepath.IsAbs(spec.Dir) {
		return errors.New("managed process directory must be absolute")
	}
	if spec.Env == nil {
		return errors.New("managed process environment is nil")
	}
	return nil
}

func initialThread(pid uint32) (windows.Handle, error) {
	threadID, err := initialThreadID(pid)
	if err != nil {
		return 0, err
	}
	return windows.OpenThread(windows.THREAD_SUSPEND_RESUME|windows.SYNCHRONIZE, false, threadID)
}

func initialThreadID(pid uint32) (uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, err
	}
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	err = windows.Thread32First(snapshot, &entry)
	var threadID uint32
	for err == nil {
		if entry.OwnerProcessID == pid {
			if threadID != 0 {
				return 0, errors.Join(
					errors.New("managed process has multiple initial threads while suspended"),
					windows.CloseHandle(snapshot),
				)
			}
			threadID = entry.ThreadID
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		err = windows.Thread32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, errors.Join(err, windows.CloseHandle(snapshot))
	}
	if threadID == 0 {
		return 0, errors.Join(
			errors.New("managed process initial thread was not found"),
			windows.CloseHandle(snapshot),
		)
	}
	if err := windows.CloseHandle(snapshot); err != nil {
		return 0, err
	}
	return threadID, nil
}

// terminateSuspendedInitialThread 只用于尚未 Resume 的创建失败路径，避免未入 Job 的进程失去 owner。
func terminateSuspendedInitialThread(pid uint32, exitCode uint32) error {
	threadID, err := initialThreadID(pid)
	if err != nil {
		return fmt.Errorf("find suspended process initial thread for termination: %w", err)
	}
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE|windows.SYNCHRONIZE, false, threadID)
	if err != nil {
		return fmt.Errorf("open suspended process initial thread for termination: %w", err)
	}
	result, _, callErr := terminateThreadProcedure.Call(uintptr(thread), uintptr(exitCode))
	var terminateErr error
	if result == 0 {
		terminateErr = callErr
		if terminateErr == nil || errors.Is(terminateErr, windows.ERROR_SUCCESS) {
			terminateErr = errors.New("terminate suspended process initial thread returned zero")
		}
	}
	return errors.Join(terminateErr, windows.CloseHandle(thread))
}

func terminateStartedProcess(processValue *os.Process, exitCode uint32) error {
	var terminateErr error
	if err := processValue.WithHandle(func(handle uintptr) {
		terminateErr = windows.TerminateProcess(windows.Handle(handle), exitCode)
	}); err != nil {
		return err
	}
	return terminateErr
}

func startedProcessExited(processValue *os.Process) (bool, error) {
	var result uint32
	var waitErr error
	if err := processValue.WithHandle(func(handle uintptr) {
		result, waitErr = windows.WaitForSingleObject(windows.Handle(handle), 0)
	}); err != nil {
		return false, err
	}
	if waitErr != nil {
		return false, waitErr
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("query managed process exit returned status %d", result)
	}
}

func ownManagedProcess(
	ctx context.Context,
	sink StreamSink,
	processValue *os.Process,
	job managedJob,
	stdout *os.File,
	stderr *os.File,
) *ManagedProcess {
	managed := &ManagedProcess{
		process:  processValue,
		job:      job,
		pid:      uint32(processValue.Pid),
		exited:   make(chan struct{}),
		waitDone: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	sink = managed.guardedSink(sink)
	sinkContext, cancelSink := context.WithCancel(context.WithoutCancel(ctx))
	settled := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		drainStream(sinkContext, StreamStdout, stdout, sink, managed.recordSinkError)
		if err := stdout.Close(); err != nil {
			managed.recordSinkError(fmt.Errorf("close managed process stdout: %w", err))
		}
	}()
	go func() {
		defer readers.Done()
		drainStream(sinkContext, StreamStderr, stderr, sink, managed.recordSinkError)
		if err := stderr.Close(); err != nil {
			managed.recordSinkError(fmt.Errorf("close managed process stderr: %w", err))
		}
	}()
	go func() {
		state, waitErr := processValue.Wait()
		close(managed.exited)
		readers.Wait()
		close(settled)
		if err := ctx.Err(); err != nil {
			managed.recordCancellation(err)
		}
		result, resultErr := processExitResult(state, waitErr)
		managed.mu.Lock()
		managed.result = result
		managed.waitErr = errors.Join(managed.waitErr, resultErr)
		managed.mu.Unlock()
		close(managed.waitDone)
	}()
	go func() {
		terminateCancelled := func(cause error) {
			managed.recordCancellation(cause)
			if err := job.Terminate(130); err != nil {
				managed.mu.Lock()
				managed.waitErr = errors.Join(managed.waitErr, fmt.Errorf("terminate cancelled managed process: %w", err))
				managed.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			terminateCancelled(ctx.Err())
			timer := time.NewTimer(managedSinkShutdownTimeout)
			select {
			case <-settled:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			cancelSink()
		case <-managed.closed:
			cancelSink()
		case <-settled:
			emptyContext, cancelEmpty := context.WithCancel(context.Background())
			empty := make(chan error, 1)
			go func() { empty <- job.waitEmpty(emptyContext) }()
			select {
			case <-ctx.Done():
				cancelEmpty()
				<-empty
				terminateCancelled(ctx.Err())
			case <-managed.closed:
				cancelEmpty()
				<-empty
			case err := <-empty:
				cancelEmpty()
				if err != nil {
					// Wait 只描述根进程与管道；Job 查询错误由显式 WaitEmpty 返回。
					select {
					case <-ctx.Done():
						terminateCancelled(ctx.Err())
					case <-managed.closed:
					}
				}
			}
			cancelSink()
		}
	}()
	return managed
}
