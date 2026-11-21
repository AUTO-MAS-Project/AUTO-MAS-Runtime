//go:build linux || darwin

package state

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSystemPIDProbe_POSIXCurrentProcessIsAlive(t *testing.T) {
	t.Parallel()

	probe := NewSystemPIDProbe()
	alive, err := probe.Alive(t.Context(), uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("Alive(current PID) error = %v", err)
	}
	if !alive {
		t.Fatal("Alive(current PID) = false, want true")
	}
}

func TestSystemPIDProbe_POSIXCreationIdentityIsStable(t *testing.T) {
	t.Parallel()

	probe := NewSystemPIDProbe()
	pid := uint32(os.Getpid())
	first, err := probe.CreatedAt(t.Context(), pid)
	if err != nil {
		t.Fatalf("CreatedAt() error = %v", err)
	}
	second, err := probe.CreatedAt(t.Context(), pid)
	if err != nil {
		t.Fatalf("CreatedAt() second call error = %v", err)
	}
	if first.IsZero() || first.After(time.Now()) {
		t.Fatalf("CreatedAt() = %v, want past nonzero time", first)
	}
	if !first.Equal(second) {
		t.Fatalf("CreatedAt() = %v then %v, want equal kernel identity", first, second)
	}
	token, err := probe.Identity(t.Context(), pid)
	if err != nil {
		t.Fatalf("Identity() error = %v", err)
	}
	if token == "" {
		t.Fatal("Identity() = empty, want kernel creation token")
	}
	again, err := probe.Identity(t.Context(), pid)
	if err != nil {
		t.Fatalf("Identity() second call error = %v", err)
	}
	if token != again {
		t.Fatalf("Identity() = %q then %q, want stable token", token, again)
	}
}

func TestSystemPIDProbe_POSIXRejectsInvalidPID(t *testing.T) {
	t.Parallel()

	probe := NewSystemPIDProbe()
	if _, err := probe.Alive(t.Context(), 0); !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("Alive(0) error = %v, want ErrInvalidPID", err)
	}
	if _, err := probe.CreatedAt(t.Context(), 0); !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("CreatedAt(0) error = %v, want ErrInvalidPID", err)
	}
	if _, err := probe.Identity(t.Context(), 0); !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("Identity(0) error = %v, want ErrInvalidPID", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probe.Alive(cancelled, uint32(os.Getpid())); !errors.Is(err, context.Canceled) {
		t.Fatalf("Alive(cancelled ctx) error = %v, want context canceled", err)
	}
}

func TestSystemPIDProbe_POSIXExitedHelperIsDead(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestSystemPIDProbe_POSIXHelperProcess$")
	command.Env = append(os.Environ(), "AUTO_MAS_POSIX_PID_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("helper Start() error = %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		// 测试失败时只回收本测试启动且尚未 Wait 的 helper。
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	reader := bufio.NewReader(stdout)
	ready := ""
	for range 8 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read helper READY error = %v", err)
		}
		if strings.TrimSpace(line) == "ready" {
			ready = line
			break
		}
	}
	if strings.TrimSpace(ready) != "ready" {
		t.Fatalf("helper READY = %q, want ready", ready)
	}

	probe := NewSystemPIDProbe()
	pid := uint32(command.Process.Pid)
	alive, err := probe.Alive(t.Context(), pid)
	if err != nil || !alive {
		t.Fatalf("Alive(helper before exit) = %t, %v, want true/nil", alive, err)
	}
	created, err := probe.CreatedAt(t.Context(), pid)
	if err != nil {
		t.Fatalf("CreatedAt(helper) error = %v", err)
	}
	if created.IsZero() || created.After(time.Now()) {
		t.Fatalf("CreatedAt(helper) = %v, want past nonzero time", created)
	}
	if _, err := fmt.Fprintln(stdin, "exit"); err != nil {
		t.Fatalf("write helper EXIT error = %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("helper stdin Close() error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper Wait() error = %v", err)
	}
	waited = true
	alive, err = probe.Alive(t.Context(), pid)
	if err != nil {
		t.Fatalf("Alive(helper after Wait) error = %v", err)
	}
	if alive {
		t.Fatal("Alive(helper after Wait) = true, want false")
	}
	if _, err := probe.CreatedAt(t.Context(), pid); err == nil {
		t.Fatal("CreatedAt(helper after Wait) error = nil, want missing process failure")
	}
}

func TestSystemPIDProbe_POSIXHelperProcess(t *testing.T) {
	if os.Getenv("AUTO_MAS_POSIX_PID_HELPER") != "1" {
		t.Skip("helper entrypoint")
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(1)
	}
}
