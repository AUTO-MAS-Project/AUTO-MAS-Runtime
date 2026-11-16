//go:build linux || darwin

package process_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

func TestPOSIXProcess_HostHelper(t *testing.T) {
	if os.Getenv("M11_HOST_HELPER") != "1" {
		return
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	proc, err := process.StartManaged(context.Background(), process.StartSpec{Executable: shell, Dir: os.TempDir(), Env: os.Environ(), Args: []string{"-c", "sleep 60 & echo ready; wait"}, Sink: func(_ context.Context, r process.StreamRecord) error {
		if r.Event == "ready" {
			ready <- struct{}{}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	members, err := proc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	pipe := os.NewFile(3, "test-ready")
	if err = json.NewEncoder(pipe).Encode(members); err != nil {
		t.Fatal(err)
	}
	if err = pipe.Close(); err != nil {
		t.Fatal(err)
	}
	<-time.NewTimer(time.Minute).C
}
func TestPOSIXProcess_HostDeath(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	host := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPOSIXProcess_HostHelper$")
	host.Env = append(os.Environ(), "M11_HOST_HELPER=1")
	host.ExtraFiles = []*os.File{w}
	if err = host.Start(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = host.Process.Kill(); _ = host.Wait() }()
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	var members []process.Info
	if err = json.NewDecoder(r).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if len(members) < 2 {
		t.Fatalf("got %d members, want root and child", len(members))
	}
	if err = host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		waitPOSIXGone(t, int(member.PID))
	}
}
func TestPOSIXProcess_DetachedHelper(t *testing.T) {
	if os.Getenv("M11_DETACH_HELPER") != "1" {
		return
	}
	child := exec.CommandContext(context.Background(), "/bin/sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Println(child.Process.Pid)
	<-time.NewTimer(time.Minute).C
}
func TestPOSIXProcess_DetachedSession(t *testing.T) {
	pidCh := make(chan int, 1)
	proc, err := process.StartManaged(t.Context(), process.StartSpec{Executable: os.Args[0], Dir: t.TempDir(), Env: append(os.Environ(), "M11_DETACH_HELPER=1"), Args: []string{"-test.run=^TestPOSIXProcess_DetachedHelper$"}, Sink: func(_ context.Context, r process.StreamRecord) error {
		if pid, e := strconv.Atoi(r.Event); e == nil {
			pidCh <- pid
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	var pid int
	select {
	case pid = <-pidCh:
	case <-time.After(5 * time.Second):
		t.Fatal("detached readiness timed out")
	}
	defer func() { _ = unix.Kill(pid, unix.SIGKILL) }()
	if err = proc.Terminate(1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = proc.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if err = unix.Kill(pid, 0); err != nil {
		t.Fatalf("detached child not alive: %v", err)
	}
}
func waitPOSIXGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		err := unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		// 无父进程可回收的僵尸不再执行；容器 PID 1 可能不承担 wait。
		if zombiePOSIX(pid) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("pid %d still alive", pid)
		case <-tick.C:
		}
	}
}

func TestPOSIXProcess_GuardianDeathFailsClosed(t *testing.T) {
	proc, err := process.StartManaged(t.Context(), process.StartSpec{Executable: "/bin/sleep", Args: []string{"60"}, Dir: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	members, err := proc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var guardian uint32
	for _, member := range members {
		if member.PID == proc.PID() {
			guardian = member.ParentPID
		}
	}
	if guardian == 0 {
		t.Fatal("guardian identity missing")
	}
	if err = unix.Kill(int(guardian), unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("guardian EOF was hidden by inherited descriptors")
	}
	if err = proc.Close(); err == nil {
		t.Fatal("got successful Close after guardian crash, want error")
	}
	waitPOSIXGone(t, int(proc.PID()))
}
