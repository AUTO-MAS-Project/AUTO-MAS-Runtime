//go:build windows

package uv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// TestRunner_ExitedProcessCannotJoinJob 固定子进程已退出、尚未 Wait 的顺序，验证真实 Windows API。
func TestRunner_ExitedProcessCannotJoinJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, testExecutable(t), "-test.run=^TestFakeUVProcess$")
	command.Env = append(os.Environ(), "FAKE_UV_STDOUT=uv 0.12.3\n")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	var status, code uint32
	var probeErr error
	if err := command.Process.WithHandle(func(handle uintptr) {
		status, probeErr = windows.WaitForSingleObject(windows.Handle(handle), 5000)
		if probeErr == nil {
			probeErr = windows.GetExitCodeProcess(windows.Handle(handle), &code)
		}
	}); err != nil || probeErr != nil || status != windows.WAIT_OBJECT_0 || code != 0 {
		t.Fatalf("child exit probe = status %d code %d error %v / %v, want signalled exit 0", status, code, err, probeErr)
	}
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Close() })
	// 先确认 OpenProcess 成功，避免把 PID/访问句柄错误误记为 AssignProcessToJobObject 错误。
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(command.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	rawJob, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(rawJob) })
	apiErr := windows.AssignProcessToJobObject(rawJob, handle)
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	assignErr := job.Assign(uint32(command.Process.Pid))
	var errno syscall.Errno
	if !errors.As(apiErr, &errno) {
		t.Fatalf("AssignProcessToJobObject error = %v, want Windows errno", apiErr)
	}
	t.Logf("OpenProcess succeeded; WaitForSingleObject=%d GetExitCodeProcess=%d AssignProcessToJobObject windowsError=%d Job.Assign=%v", status, code, errno, assignErr)
	if !errors.Is(apiErr, windows.ERROR_ACCESS_DENIED) || !errors.Is(assignErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("assign exited child = API %v wrapper %v, want ERROR_ACCESS_DENIED (5)", apiErr, assignErr)
	}
}

type observedIsolationJob struct {
	process.Job
	t *testing.T
}

func (j observedIsolationJob) Assign(pid uint32) error {
	j.t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	status, waitErr := windows.WaitForSingleObject(handle, 200)
	var code uint32
	codeErr := windows.GetExitCodeProcess(handle, &code)
	closeErr := windows.CloseHandle(handle)
	j.t.Logf("before Job.Assign: pid=%d WaitForSingleObject=%d exitCode=%d", pid, status, code)
	if waitErr != nil || codeErr != nil || closeErr != nil {
		return errors.Join(waitErr, codeErr, closeErr)
	}
	if status != uint32(windows.WAIT_TIMEOUT) {
		j.t.Errorf("child already executed before Job.Assign: wait=%d exitCode=%d, want suspended", status, code)
	}
	return j.Job.Assign(pid)
}

func TestRunner_AssignsBeforeExecuting(t *testing.T) {
	runner := newTestRunner(t)
	runner.newJob = func() (runnerJob, error) {
		job, err := process.NewJob()
		return observedIsolationJob{Job: job, t: t}, err
	}
	result, err := runner.Run(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
		Stage:       protocol.StageUVVerify,
		Environment: map[string]string{"FAKE_UV_STDOUT": "uv 0.12.3\n", "FAKE_UV_STDERR": "probe stderr\n"},
	})
	if err != nil || result.ExitCode != 0 || result.Stdout != "uv 0.12.3\n" || result.Stderr != "probe stderr\n" {
		t.Fatalf("Run() = %+v, %v, want complete output and exit 0", result, err)
	}
}

type cancelBeforeResumeJob struct {
	process.Job
	cancel     context.CancelFunc
	pid        uint32
	handle     windows.Handle
	closeError error
}

func (j *cancelBeforeResumeJob) Close() error {
	return errors.Join(j.Job.Close(), j.closeError)
}

func (j *cancelBeforeResumeJob) Assign(pid uint32) error {
	j.pid = pid
	var err error
	j.handle, err = windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	if err := j.Job.Assign(pid); err != nil {
		return err
	}
	j.cancel()
	return nil
}

func TestRunner_CancelBeforeResumeDoesNotExecute(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := &cancelBeforeResumeJob{cancel: cancel}
	runner.newJob = func() (runnerJob, error) {
		var err error
		job.Job, err = process.NewJob()
		return job, err
	}
	recordPath := filepath.Join(t.TempDir(), "must-not-exist.txt")
	_, err := runner.Run(ctx, []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
		Stage: protocol.StageUVVerify, Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
	})
	if job.handle == 0 {
		t.Fatal("child handle was not captured at assignment")
	}
	t.Cleanup(func() { _ = windows.CloseHandle(job.handle) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child executed while cancelling: marker error = %v", statErr)
	}
	status, waitErr := windows.WaitForSingleObject(job.handle, 0)
	if waitErr != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("child %d after Run = wait %d error %v, want confirmed exited", job.pid, status, waitErr)
	}
}

func TestRunner_CancelBeforeResumePreservesCleanupError(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cleanupError := errors.New("job close failed")
	job := &cancelBeforeResumeJob{cancel: cancel, closeError: cleanupError}
	runner.newJob = func() (runnerJob, error) {
		var err error
		job.Job, err = process.NewJob()
		return job, err
	}
	t.Cleanup(func() {
		if job.handle != 0 {
			_ = windows.CloseHandle(job.handle)
		}
	})
	_, err := runner.Run(ctx, []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{Stage: protocol.StageUVVerify})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupError) {
		t.Fatalf("Run() error = %v, want cancellation and job cleanup error", err)
	}
}

func TestRunner_ResumeFailurePreservesCancellation(t *testing.T) {
	for _, test := range []struct {
		name         string
		wantContext  error
		wantCode     protocol.Code
		wantExitCode protocol.ExitCode
	}{
		{name: "cancel", wantContext: context.Canceled, wantCode: protocol.CodeOperationCancelled, wantExitCode: protocol.ExitCodeOperationCancelled},
		{name: "deadline", wantContext: context.DeadlineExceeded, wantCode: protocol.CodeOperationCancelled, wantExitCode: protocol.ExitCodeOperationCancelled},
		{name: "uncancelled", wantCode: protocol.CodeUVExecFailed, wantExitCode: protocol.ExitCodeEnvironmentFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newTestRunner(t)
			ctx, cancel := context.WithCancel(t.Context())
			if test.name == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			}
			defer cancel()
			cleanupError := errors.New("job close failed after actual close")
			job := &cancelBeforeResumeJob{cancel: func() {}, closeError: cleanupError}
			runner.newJob = func() (runnerJob, error) {
				var err error
				job.Job, err = process.NewJob()
				return job, err
			}
			t.Cleanup(func() {
				if job.handle != 0 {
					_ = windows.CloseHandle(job.handle)
				}
			})
			var resumeError error
			var waitStatus uint32
			var waitError error
			runner.resumeProcess = func(child *os.Process) error {
				// 调用点已越过 Run 的取消检查，只控制外部进程恢复前的时序。
				switch test.name {
				case "cancel":
					cancel()
				case "deadline":
					<-ctx.Done()
				case "uncancelled":
					if err := child.Kill(); err != nil {
						return err
					}
				}
				waitStatus, waitError = windows.WaitForSingleObject(job.handle, 5000)
				resumeError = process.ResumeSuspended(child)
				return resumeError
			}
			recordPath := filepath.Join(t.TempDir(), "must-not-exist.txt")
			_, err := runner.Run(ctx, []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
				Stage: protocol.StageUVVerify, Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
			})
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("Run() error = %v, want structured uv error", err)
			}
			definition, ok := protocol.LookupErrorDefinition(failure.Code())
			t.Logf("Run code=%s exitCode=%d ctx=%v resume=%v wait=%d", failure.Code(), definition.ExitCode, ctx.Err(), resumeError, waitStatus)
			if failure.Code() != test.wantCode || !ok || definition.ExitCode != test.wantExitCode {
				t.Errorf("Run() code=%s exitCode=%d, want %s / %d", failure.Code(), definition.ExitCode, test.wantCode, test.wantExitCode)
			}
			if !errors.Is(ctx.Err(), test.wantContext) || test.wantContext != nil && !errors.Is(err, test.wantContext) {
				t.Errorf("Run() error = %v ctx=%v, want context %v in chain", err, ctx.Err(), test.wantContext)
			}
			if waitError != nil || waitStatus != windows.WAIT_OBJECT_0 || resumeError == nil {
				t.Fatalf("resume boundary = wait %d error %v resume %v, want exited child and real resume failure", waitStatus, waitError, resumeError)
			}
			var exitError *exec.ExitError
			if !errors.Is(err, resumeError) || !errors.Is(err, cleanupError) || !errors.As(err, &exitError) {
				t.Errorf("Run() error = %v, want original resume, job cleanup and Wait errors", err)
			}
			if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("suspended child executed: marker error = %v", statErr)
			}
			if test.wantContext == nil && (failure.Details()["failureKind"] != "process_isolation_failed" || failure.DiagnosticDetails()["operation"] != "resume_process") {
				t.Errorf("uncancelled failure details = %v / %v, want isolation failure", failure.Details(), failure.DiagnosticDetails())
			}
		})
	}
}

func TestRunner_CancelReapsDescendants(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var descendant windows.Handle
	_, err := runner.Run(ctx, []string{"-test.run=^TestRunnerTreeProcess$"}, RunOptions{
		Stage: protocol.StageUVVerify, Environment: map[string]string{"FAKE_UV_TREE": "root"},
		Line: func(_ context.Context, stream, line string) error {
			if stream != "stdout" || !strings.HasPrefix(line, "descendant=") {
				return nil
			}
			pid, err := strconv.ParseUint(strings.TrimPrefix(line, "descendant="), 10, 32)
			if err != nil {
				return err
			}
			descendant, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			if err == nil {
				cancel()
			}
			return err
		},
	})
	if descendant == 0 {
		t.Fatalf("descendant handle was not captured: %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(descendant) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	status, waitErr := windows.WaitForSingleObject(descendant, 5000)
	if waitErr != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant after cancellation = wait %d error %v, want confirmed exited", status, waitErr)
	}
}

// TestRunnerTreeProcess 提供真实父子进程，输出 PID 后由 Job 取消回收。
func TestRunnerTreeProcess(t *testing.T) {
	role := os.Getenv("FAKE_UV_TREE")
	if role == "" {
		return
	}
	if role == "root" {
		child := exec.CommandContext(t.Context(), testExecutable(t), "-test.run=^TestRunnerTreeProcess$")
		child.Env = append(os.Environ(), "FAKE_UV_TREE=descendant")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stdout.WriteString("descendant=" + strconv.Itoa(child.Process.Pid) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	<-timer.C
	os.Exit(0)
}
