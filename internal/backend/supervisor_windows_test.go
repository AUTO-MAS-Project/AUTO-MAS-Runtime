//go:build windows

package backend

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestBackendManaged_WindowsAccessDeniedReusedPID(t *testing.T) {
	f := newBackendFixture(t)
	startedAt := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	f.state.transaction = &Transaction{PID: 7331, StartedAt: startedAt, Handle: &fakeTransaction{}}
	f.pid = &fakePID{
		aliveErr:  &state.PIDProbeError{Operation: "open-process", PID: 7331, Cause: windows.ERROR_ACCESS_DENIED},
		createdAt: startedAt.Add(time.Hour),
	}
	if err := f.supervisor().recoverStaleTransaction(t.Context()); err != nil {
		t.Fatalf("recoverStaleTransaction() = %v, want nil", err)
	}
	if f.pid.creationCalls != 1 || f.state.removeCalls != 1 {
		t.Fatalf("query/remove calls = %d/%d, want 1/1", f.pid.creationCalls, f.state.removeCalls)
	}
}
