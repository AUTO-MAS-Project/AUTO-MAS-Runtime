//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	managedChildRoleEnv         = "AUTO_MAS_TEST_MANAGED_CHILD_ROLE"
	managedChildSignalEnv       = "AUTO_MAS_TEST_MANAGED_CHILD_SIGNAL"
	managedChildReleaseEnv      = "AUTO_MAS_TEST_MANAGED_CHILD_RELEASE"
	managedGrandchildPIDEnv     = "AUTO_MAS_TEST_MANAGED_GRANDCHILD_PID"
	managedGrandchildReleaseEnv = "AUTO_MAS_TEST_MANAGED_GRANDCHILD_RELEASE"
	managedDetachGrandchildEnv  = "AUTO_MAS_TEST_MANAGED_DETACH_GRANDCHILD"
	// managedBreakawayGrandchildEnv 让 detached spawner 用 CREATE_BREAKAWAY_FROM_JOB
	// 启动孙进程，模拟 AUTO-MAS 拉起模拟器与 PC 游戏的方式（增补 1 C8）。
	managedBreakawayGrandchildEnv   = "AUTO_MAS_TEST_MANAGED_BREAKAWAY_GRANDCHILD"
	managedChildRootRole            = "root"
	managedChildSpawnerRole         = "spawner"
	managedChildDetachedSpawnerRole = "detached-spawner"
	managedChildGrandchildRole      = "grandchild"
)

func TestJob_CreateSuspendedAssignsBeforeResume(t *testing.T) {
	managed, signal, release := startTestManaged(t.Context(), t, managedChildRootRole)
	defer cleanupTestManaged(t, managed)
	record := waitTestSignal(t, signal)
	if !strings.Contains(record, "inJob=true") {
		t.Fatalf("child record = %q, want inJob=true", record)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitManagedSuccess(t, managed)
}

func TestJob_ConfiguresKillOnCloseAndNoWindow(t *testing.T) {
	operations := defaultWindowsStartOperations()
	var creationFlags uint32
	operations.startProcess = func(name string, args []string, attr *os.ProcAttr) (*os.Process, error) {
		system := attr.Sys
		if system == nil {
			return nil, errors.New("missing Windows process attributes")
		}
		creationFlags = system.CreationFlags
		return os.StartProcess(name, args, attr)
	}
	spec, signal, _ := testManagedSpec(t, managedChildRootRole)
	managed, err := startManagedWindows(t.Context(), spec, operations)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTestSignal(t, signal)
	if creationFlags&(windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW) != windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW {
		t.Fatalf("creation flags = %#x", creationFlags)
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = managed.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
}

func TestJob_AssignFailureCleansHandles(t *testing.T) {
	operations := defaultWindowsStartOperations()
	want := errors.New("assign injection")
	var pid int
	operations.startProcess = captureStartedPID(&pid, operations.startProcess)
	operations.assign = func(managedJob, *os.Process) error { return want }
	spec, signal, _ := testManagedSpec(t, managedChildRootRole)
	managed, err := startManagedWindows(t.Context(), spec, operations)
	if managed != nil || !errors.Is(err, want) {
		t.Fatalf("StartManaged() = %#v, %v", managed, err)
	}
	if _, statErr := os.Stat(signal); !os.IsNotExist(statErr) {
		t.Fatalf("suspended child ran before failed assignment: %v", statErr)
	}
	waitTestPIDExit(t, pid)
}

func TestJob_ResumeFailureCleansJob(t *testing.T) {
	operations := defaultWindowsStartOperations()
	want := errors.New("resume injection")
	var pid int
	operations.startProcess = captureStartedPID(&pid, operations.startProcess)
	operations.resumeThread = func(windows.Handle) (uint32, error) { return 0, want }
	spec, signal, _ := testManagedSpec(t, managedChildRootRole)
	managed, err := startManagedWindows(t.Context(), spec, operations)
	if managed != nil || !errors.Is(err, want) {
		t.Fatalf("StartManaged() = %#v, %v", managed, err)
	}
	if _, statErr := os.Stat(signal); !os.IsNotExist(statErr) {
		t.Fatalf("suspended child ran after failed resume: %v", statErr)
	}
	waitTestPIDExit(t, pid)
}

func TestJob_ContextCancellationDrainsAndClosesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	spec, signal, _ := testManagedSpec(t, managedChildRootRole)
	sinkStarted := make(chan struct{})
	var sinkStartedOnce sync.Once
	spec.Sink = func(ctx context.Context, _ StreamRecord) error {
		sinkStartedOnce.Do(func() { close(sinkStarted) })
		<-ctx.Done()
		return ctx.Err()
	}
	managed, err := StartManaged(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTestSignal(t, signal)
	select {
	case <-sinkStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not receive child output")
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer waitCancel()
	_, err = managed.Wait(waitCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if err := managed.WaitEmpty(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJob_CloseCancelsBlockedSink(t *testing.T) {
	spec, signal, _ := testManagedSpec(t, managedChildRootRole)
	sinkStarted := make(chan struct{})
	var sinkStartedOnce sync.Once
	spec.Sink = func(ctx context.Context, _ StreamRecord) error {
		sinkStartedOnce.Do(func() { close(sinkStarted) })
		<-ctx.Done()
		return ctx.Err()
	}
	managed, err := StartManaged(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTestSignal(t, signal)
	select {
	case <-sinkStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not receive child output")
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	waitContext, cancelWait := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelWait()
	if _, err := managed.Wait(waitContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want cancelled sink", err)
	}
}

func TestJob_PipeSinkFailureDrainsAndStopsCallbacks(t *testing.T) {
	want := errors.New("sink injection")
	var calls atomic.Int32
	spec, _, _ := testManagedSpec(t, managedChildRootRole)
	spec.Sink = func(context.Context, StreamRecord) error {
		calls.Add(1)
		return want
	}
	managed, err := StartManaged(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupTestManaged(t, managed)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = managed.Wait(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("Wait() error = %v, want sink cause", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("sink calls = %d, want 1", calls.Load())
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestJob_ChildStdinCannotConsumeRuntimeControl(t *testing.T) {
	managed, signal, release := startTestManaged(t.Context(), t, managedChildRootRole)
	defer cleanupTestManaged(t, managed)
	record := waitTestSignal(t, signal)
	if !strings.Contains(record, "stdinEOF=true") {
		t.Fatalf("child record = %q, want stdinEOF=true", record)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitManagedSuccess(t, managed)
}

func TestJob_QueryConfirmsTreeEmpty(t *testing.T) {
	managed, signal, release := startTestManaged(t.Context(), t, managedChildRootRole)
	defer cleanupTestManaged(t, managed)
	_ = waitTestSignal(t, signal)
	snapshot, err := managed.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotContains(snapshot, managed.PID()) {
		t.Fatalf("snapshot = %#v, want root PID %d", snapshot, managed.PID())
	}
	rootInfo := snapshotByPID(t, snapshot, managed.PID())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.ParentPID != uint32(os.Getpid()) || !strings.EqualFold(rootInfo.Executable, filepath.Clean(executable)) {
		t.Fatalf("root identity = %#v", rootInfo)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitManagedSuccess(t, managed)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = managed.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 0 {
		t.Fatalf("snapshot after exit = %#v", snapshot)
	}
}

// TestJob_SnapshotAfterRootExitReportsEmptyTreeWithoutError 锁定一个曾造成
// BACKEND_FORCE_TERMINATED 系统性误报的过渡态：snapshot 先查 Job 的 pid 列表、
// 再查 Toolhelp32，刚退出的根进程会出现在前者而不在后者。那不是故障，是「成员在
// 两次查询之间退出了」，必须当成已退出跳过，而不是让整个快照失败。
func TestJob_SnapshotAfterRootExitReportsEmptyTreeWithoutError(t *testing.T) {
	// 该竞态与调度相关，重复若干轮以免偶然的时序掩盖回归。
	for attempt := range 5 {
		spec, signal, release := testManagedSpec(t, managedChildRootRole)
		managed, err := StartManaged(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		_ = waitTestSignal(t, signal)
		if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
			t.Fatal(err)
		}
		<-managed.Exited()
		members, snapshotErr := managed.Snapshot()
		if snapshotErr != nil {
			t.Fatalf("attempt %d: Snapshot() right after root exit error = %v, want nil", attempt, snapshotErr)
		}
		for _, member := range members {
			if member.PID == managed.PID() {
				continue
			}
			t.Fatalf("attempt %d: snapshot after root exit = %#v, want no surviving descendant", attempt, members)
		}
		waitManagedSuccess(t, managed)
		if err := managed.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJobE2E_FastChildSpawnIsAlreadyInJob(t *testing.T) {
	managed, signal, _ := startTestManaged(t.Context(), t, managedChildSpawnerRole)
	defer cleanupTestManaged(t, managed)
	_ = waitTestSignal(t, signal)
	grandchildPID := waitGrandchildPID(t, filepath.Join(filepath.Dir(signal), "grandchild.pid"))
	snapshot, err := managed.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotContains(snapshot, managed.PID()) || !snapshotContains(snapshot, uint32(grandchildPID)) {
		t.Fatalf("snapshot = %#v, want root %d and child %d", snapshot, managed.PID(), grandchildPID)
	}
	grandchildInfo := snapshotByPID(t, snapshot, uint32(grandchildPID))
	if grandchildInfo.ParentPID != managed.PID() || !filepath.IsAbs(grandchildInfo.Executable) {
		t.Fatalf("grandchild identity = %#v", grandchildInfo)
	}
}

func TestJob_ExitedPrecedesDescendantPipeEOF(t *testing.T) {
	spec, signal, release := testManagedSpec(t, managedChildSpawnerRole)
	spec.Env = replaceTestEnvironment(spec.Env, managedDetachGrandchildEnv, "1")
	managed, err := StartManaged(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupTestManaged(t, managed)
	_ = waitTestSignal(t, signal)
	grandchildPID := waitGrandchildPID(t, filepath.Join(filepath.Dir(signal), "grandchild.pid"))
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-managed.Exited():
	case <-time.After(3 * time.Second):
		t.Fatal("root exit notification waited for descendant-held pipe")
	}
	shortCtx, shortCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, waitErr := managed.Wait(shortCtx)
	shortCancel()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want descendant-held pipe timeout", waitErr)
	}
	if err := managed.Terminate(98); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := managed.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	waitTestPIDExit(t, grandchildPID)
}

func TestJobE2E_RuntimeTerminationReapsGrandchildren(t *testing.T) {
	managed, signal, _ := startTestManaged(t.Context(), t, managedChildSpawnerRole)
	_ = waitTestSignal(t, signal)
	grandchildPID := waitGrandchildPID(t, filepath.Join(filepath.Dir(signal), "grandchild.pid"))
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(grandchildPID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, _ = managed.Wait(ctx)
	result, err := windows.WaitForSingleObject(handle, 3000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("grandchild was not reaped: result=%d err=%v", result, err)
	}
}

// TestJob_BreakawayGrandchildSurvivesJobClose 证明增补 1 C8 的正面：显式带
// CREATE_BREAKAWAY_FROM_JOB 的孙进程不属于 Runtime 的 Job，Job 关闭后仍存活。
// 这是「关闭 AUTO-MAS 时模拟器与 PC 游戏不跟着关」的机制依据。
func TestJob_BreakawayGrandchildSurvivesJobClose(t *testing.T) {
	handle, jobHandle, managed, release := startDetachedGrandchildFixture(t, true)
	if processInJob(t, handle, jobHandle) {
		t.Fatal("breakaway grandchild is still inside the runtime job")
	}
	if !processInJob(t, handle, 0) {
		t.Fatal("breakaway grandchild belongs to no job at all, want its own or none of ours")
	}
	closeManagedAfterRelease(t, managed, release)
	if result, err := windows.WaitForSingleObject(handle, 500); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("breakaway grandchild after job close = result %d, err %v, want WAIT_TIMEOUT", result, err)
	}
}

// TestJob_NonBreakawayGrandchildStaysInJobAndIsReaped 是上一条的对照组：同一个
// spawner、同样不继承管道，只是不带 CREATE_BREAKAWAY_FROM_JOB。BREAKAWAY_OK 只
// 允许显式请求脱离，未请求的进程归属与回收一个字节都不能变（红线第 5 条）。
func TestJob_NonBreakawayGrandchildStaysInJobAndIsReaped(t *testing.T) {
	handle, jobHandle, managed, release := startDetachedGrandchildFixture(t, false)
	if !processInJob(t, handle, jobHandle) {
		t.Fatal("grandchild without CREATE_BREAKAWAY_FROM_JOB escaped the runtime job")
	}
	closeManagedAfterRelease(t, managed, release)
	if result, err := windows.WaitForSingleObject(handle, 3000); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("grandchild after job close = result %d, err %v, want WAIT_OBJECT_0", result, err)
	}
}

// startDetachedGrandchildFixture 启动一个 detached spawner 并返回孙进程句柄、
// 当前 Job 句柄、受管进程和 release 文件路径。孙进程句柄与它的终止都由 Cleanup 负责，
// 因为脱离出去的进程按定义不再受 Job 回收。
func startDetachedGrandchildFixture(t *testing.T, breakaway bool) (windows.Handle, windows.Handle, *ManagedProcess, string) {
	t.Helper()
	spec, signal, release := testManagedSpec(t, managedChildDetachedSpawnerRole)
	spec.Env = replaceTestEnvironment(spec.Env, managedBreakawayGrandchildEnv, boolFlagValue(breakaway))
	managed, err := StartManaged(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = managed.Close() })
	record := waitTestSignal(t, signal)
	if !strings.Contains(record, "grandchildStart=ok") {
		t.Fatalf("detached spawner record = %q, want grandchildStart=ok", record)
	}
	grandchildPID := waitGrandchildPID(t, filepath.Join(filepath.Dir(signal), "grandchild.pid"))
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false,
		uint32(grandchildPID),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 只终止仍在运行的那一个；对照组已被 Job 回收，这里的失败无意义。
		if result, waitErr := windows.WaitForSingleObject(handle, 0); waitErr == nil && result != windows.WAIT_OBJECT_0 {
			_ = windows.TerminateProcess(handle, 99)
			_, _ = windows.WaitForSingleObject(handle, 3000)
		}
		_ = windows.CloseHandle(handle)
	})
	return handle, managedJobHandle(t, managed), managed, release
}

func boolFlagValue(enabled bool) string {
	if enabled {
		return "1"
	}
	return "0"
}

// closeManagedAfterRelease 放行根进程并关闭 Job，触发 KILL_ON_JOB_CLOSE。
func closeManagedAfterRelease(t *testing.T, managed *ManagedProcess, release string) {
	t.Helper()
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := managed.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
}

// managedJobHandle 取出受管 Job 的原始句柄，供 IsProcessInJob 精确断言归属。
// 必须在 Close 之前调用：Close 会关闭该句柄。
func managedJobHandle(t *testing.T, managed *ManagedProcess) windows.Handle {
	t.Helper()
	job, ok := managed.job.(*windowsJob)
	if !ok {
		t.Fatalf("managed job type = %T, want *windowsJob", managed.job)
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.closed {
		t.Fatal("managed job is already closed")
	}
	return job.handle
}

// processInJob 报告进程是否属于指定 Job；jobHandle 为 0 时表示「是否属于任何 Job」。
func processInJob(t *testing.T, processHandle, jobHandle windows.Handle) bool {
	t.Helper()
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	var inJob uint32
	result, _, callErr := procedure.Call(
		uintptr(processHandle),
		uintptr(jobHandle),
		uintptr(unsafe.Pointer(&inJob)),
	)
	if result == 0 {
		t.Fatalf("IsProcessInJob() failed: %v", callErr)
	}
	return inJob != 0
}

func TestManagedProcessChild(t *testing.T) {
	role := os.Getenv(managedChildRoleEnv)
	if role == "" {
		return
	}
	if role == managedChildGrandchildRole {
		waitForReleaseFile(os.Getenv(managedChildReleaseEnv))
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "managed-child-started")
	inJob, err := currentProcessInJob()
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	_, stdinErr := os.Stdin.Read(buffer)
	stdinEOF := errors.Is(stdinErr, os.ErrClosed) || errors.Is(stdinErr, syscall.ERROR_BROKEN_PIPE) || stdinErr != nil
	if role == managedChildSpawnerRole {
		startManagedGrandchild(t)
	}
	record := fmt.Sprintf("inJob=%t stdinEOF=%t", inJob, stdinEOF)
	if role == managedChildDetachedSpawnerRole {
		// 启动结果写进 signal 而不是 t.Fatal：没有 BREAKAWAY_OK 时 CreateProcess
		// 直接失败，父测试必须看到原因，而不是干等 grandchild.pid 超时。
		record += " grandchildStart=" + startDetachedGrandchild()
	}
	if err := writeTestSignal(os.Getenv(managedChildSignalEnv), []byte(record+"\n")); err != nil {
		t.Fatal(err)
	}
	waitForReleaseFile(os.Getenv(managedChildReleaseEnv))
}

// startDetachedGrandchild 启动一个既不继承 Runtime 管道、也不随父测试进程取消的孙进程，
// 并在 managedBreakawayGrandchildEnv=1 时追加 CREATE_BREAKAWAY_FROM_JOB。
// 不继承 stdout/stderr 是必要的：脱离 Job 之后 Terminate 收不到它，若它仍持有写端，
// 父进程的 Wait 会永远等不到 EOF，测出来的就不是脱离行为而是管道行为。
// 返回值是写进 signal 的单行状态，"ok" 表示启动成功。
func startDetachedGrandchild() string {
	executable, err := os.Executable()
	if err != nil {
		return describeDetachedGrandchildError(err)
	}
	command := exec.Command(executable, "-test.run=^TestManagedProcessChild$")
	command.Env = replaceTestEnvironment(os.Environ(), managedChildRoleEnv, managedChildGrandchildRole)
	command.Env = replaceTestEnvironment(command.Env, managedChildReleaseEnv, os.Getenv(managedGrandchildReleaseEnv))
	if os.Getenv(managedBreakawayGrandchildEnv) == "1" {
		command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	}
	if err := command.Start(); err != nil {
		return describeDetachedGrandchildError(err)
	}
	pidFile := os.Getenv(managedGrandchildPIDEnv)
	if err := writeTestSignal(pidFile, []byte(strconv.Itoa(command.Process.Pid)+"\n")); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return describeDetachedGrandchildError(err)
	}
	go func() { _ = command.Wait() }()
	return "ok"
}

func describeDetachedGrandchildError(err error) string {
	return strings.NewReplacer("\n", " ", "\r", " ", " ", "_").Replace(err.Error())
}

func startManagedGrandchild(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if os.Getenv(managedDetachGrandchildEnv) != "1" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		t.Cleanup(cancel)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestManagedProcessChild$")
	command.Env = replaceTestEnvironment(os.Environ(), managedChildRoleEnv, managedChildGrandchildRole)
	command.Env = replaceTestEnvironment(command.Env, managedChildReleaseEnv, os.Getenv(managedGrandchildReleaseEnv))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pidFile := os.Getenv(managedGrandchildPIDEnv)
	if err := writeTestSignal(pidFile, []byte(strconv.Itoa(command.Process.Pid)+"\n")); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	go func() { _ = command.Wait() }()
}

func startTestManaged(ctx context.Context, t *testing.T, role string) (*ManagedProcess, string, string) {
	t.Helper()
	spec, signal, release := testManagedSpec(t, role)
	managed, err := StartManaged(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	return managed, signal, release
}

func testManagedSpec(t *testing.T, role string) (StartSpec, string, string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	signal := filepath.Join(root, "started.txt")
	release := filepath.Join(root, "release.txt")
	grandchildPID := filepath.Join(root, "grandchild.pid")
	grandchildRelease := filepath.Join(root, "grandchild-release.txt")
	environment := replaceTestEnvironment(os.Environ(), managedChildRoleEnv, role)
	environment = replaceTestEnvironment(environment, managedChildSignalEnv, signal)
	environment = replaceTestEnvironment(environment, managedChildReleaseEnv, release)
	environment = replaceTestEnvironment(environment, managedGrandchildPIDEnv, grandchildPID)
	environment = replaceTestEnvironment(environment, managedGrandchildReleaseEnv, grandchildRelease)
	return StartSpec{
		Executable: executable,
		Args:       []string{"-test.run=^TestManagedProcessChild$"},
		Dir:        root,
		Env:        environment,
	}, signal, release
}

func captureStartedPID(pid *int, start func(string, []string, *os.ProcAttr) (*os.Process, error)) func(string, []string, *os.ProcAttr) (*os.Process, error) {
	return func(name string, args []string, attr *os.ProcAttr) (*os.Process, error) {
		processValue, err := start(name, args, attr)
		if err == nil {
			*pid = processValue.Pid
		}
		return processValue, err
	}
}

func cleanupTestManaged(t *testing.T, managed *ManagedProcess) {
	t.Helper()
	if managed == nil {
		return
	}
	if err := managed.Terminate(98); err != nil {
		t.Errorf("terminate managed process: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Second)
	defer cancel()
	if _, err := managed.Wait(ctx); errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait managed process: %v", err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Errorf("wait managed process tree empty: %v", err)
	}
	if err := managed.Close(); err != nil {
		t.Errorf("close managed process: %v", err)
	}
}

func waitManagedSuccess(t *testing.T, managed *ManagedProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := managed.Wait(ctx)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Wait() = %#v, %v", result, err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitTestSignal(t *testing.T, path string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		payload, err := os.ReadFile(path)
		if err == nil && len(payload) > 0 && payload[len(payload)-1] == '\n' {
			return string(payload)
		}
		select {
		case <-timer.C:
			t.Fatalf("wait signal %s: %v", path, err)
		case <-ticker.C:
		}
	}
}

func waitGrandchildPID(t *testing.T, path string) int {
	t.Helper()
	payload := waitTestSignal(t, path)
	pid, err := strconv.Atoi(strings.TrimSpace(payload))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild PID = %q, err=%v", payload, err)
	}
	return pid
}

func waitTestPIDExit(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		t.Fatal("captured PID is invalid")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	result, err := windows.WaitForSingleObject(handle, 3000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("process %d did not exit: result=%d err=%v", pid, result, err)
	}
}

func snapshotContains(snapshot []Info, pid uint32) bool {
	for _, processValue := range snapshot {
		if processValue.PID == pid {
			return true
		}
	}
	return false
}

func snapshotByPID(t *testing.T, snapshot []Info, pid uint32) Info {
	t.Helper()
	for _, processValue := range snapshot {
		if processValue.PID == pid {
			return processValue
		}
	}
	t.Fatalf("snapshot does not contain PID %d: %#v", pid, snapshot)
	return Info{}
}

func replaceTestEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		entryKey, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(entryKey, key) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, key+"="+value)
}

func waitForReleaseFile(path string) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-timer.C:
			return
		case <-ticker.C:
		}
	}
}

func writeTestSignal(path string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".managed-signal-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func currentProcessInJob() (bool, error) {
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	var inJob uint32
	result, _, callErr := procedure.Call(
		uintptr(windows.CurrentProcess()),
		0,
		uintptr(unsafe.Pointer(&inJob)),
	)
	if result == 0 {
		return false, callErr
	}
	return inJob != 0, nil
}

// TestJob_SnapshotSkipsMemberExitingDuringImageQuery 锁定另一种过渡态：成员出现在
// Job 的 pid 列表与 Toolhelp32 快照里，却在随后的映像查询时正在消亡。错误码不止
// ERROR_INVALID_PARAMETER 一种，所以判据改问进程是否已收到信号：已退出就跳过，
// 仍存活才让整个快照失败。后端启动期的短命子进程（如 `git version`）落在这个窗口里，
// 让快照失败会把一次正常启动误判成 BACKEND_HEALTH_INVALID。
func TestJob_SnapshotSkipsMemberExitingDuringImageQuery(t *testing.T) {
	managed, signal, _ := startTestManaged(t.Context(), t, managedChildRootRole)
	defer cleanupTestManaged(t, managed)
	_ = waitTestSignal(t, signal)

	originalImagePath, originalExited := processImagePathFn, processExitedFn
	t.Cleanup(func() { processImagePathFn, processExitedFn = originalImagePath, originalExited })

	// 映像查询失败，错误码不是 ERROR_INVALID_PARAMETER。
	queryErr := fmt.Errorf("query image: %w", windows.ERROR_ACCESS_DENIED)
	processImagePathFn = func(uint32) (string, error) { return "", queryErr }

	// 进程已退出：跳过该成员，快照本身成功。
	processExitedFn = func(uint32) (bool, error) { return true, nil }
	members, err := managed.Snapshot()
	if err != nil {
		t.Fatalf("成员已退出时快照应成功，got %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("已退出的成员必须被跳过，got %#v", members)
	}

	// 进程仍存活：这才是真故障，必须上报。
	processExitedFn = func(uint32) (bool, error) { return false, nil }
	if _, err = managed.Snapshot(); err == nil {
		t.Fatal("成员仍存活时映像查询失败必须上报，不能静默跳过")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("上报的错误应保留原因，got %v", err)
	}
}
