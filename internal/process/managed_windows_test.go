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
	managedChildRootRole        = "root"
	managedChildSpawnerRole     = "spawner"
	managedChildGrandchildRole  = "grandchild"
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
	if err := writeTestSignal(os.Getenv(managedChildSignalEnv), []byte(record+"\n")); err != nil {
		t.Fatal(err)
	}
	waitForReleaseFile(os.Getenv(managedChildReleaseEnv))
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
