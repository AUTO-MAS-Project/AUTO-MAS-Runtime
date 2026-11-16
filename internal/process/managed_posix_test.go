//go:build linux || darwin

package process_test

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

func TestMain(m *testing.M) {
	if handled, code := process.RunInternalGuardian(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
func TestPOSIXProcess_StartAndTerminate(t *testing.T) {
	executable, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	unrelated := exec.CommandContext(t.Context(), "/bin/sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	started := make(chan struct{}, 1)
	proc, err := process.StartManaged(t.Context(), process.StartSpec{Executable: executable, Dir: t.TempDir(), Env: os.Environ(), Args: []string{"-c", "echo ready; sleep 60 & wait"}, Sink: func(_ context.Context, r process.StreamRecord) error {
		if r.Event == "ready" {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proc.Close(); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend readiness timed out")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	readyDeadline := time.NewTimer(5 * time.Second)
	defer readyDeadline.Stop()
	for {
		infos, snapshotErr := proc.Snapshot()
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if len(infos) >= 2 {
			break
		}
		select {
		case <-readyDeadline.C:
			t.Fatalf("got %+v, want managed root and descendant", infos)
		case <-ticker.C:
		}
	}
	if err = proc.Terminate(1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = proc.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = proc.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process affected: %v", err)
	}
}
