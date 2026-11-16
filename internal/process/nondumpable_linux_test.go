package process_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

func TestPOSIXProcess_NondumpableHelper(t *testing.T) {
	if os.Getenv("M11_NONDUMPABLE_HELPER") != "1" {
		return
	}
	if err := unix.Setpgid(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	<-time.NewTimer(time.Minute).C
}
func TestPOSIXProcess_UnknownExecutableStillReaped(t *testing.T) {
	ready := make(chan struct{}, 1)
	proc, err := process.StartManaged(t.Context(), process.StartSpec{Executable: os.Args[0], Dir: t.TempDir(), Env: append(os.Environ(), "M11_NONDUMPABLE_HELPER=1"), Args: []string{"-test.run=^TestPOSIXProcess_NondumpableHelper$"}, Sink: func(_ context.Context, r process.StreamRecord) error {
		if r.Event == "ready" {
			ready <- struct{}{}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("nondumpable readiness timed out")
	}
	members, err := proc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, member := range members {
		if member.PID == proc.PID() {
			found = true
		}
	}
	if !found {
		t.Fatal("known session member missing when executable path unavailable")
	}
	if err = proc.Terminate(1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = proc.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if err = proc.Close(); err != nil {
		t.Fatal(err)
	}
}
