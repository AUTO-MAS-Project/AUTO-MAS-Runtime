//go:build windows

package uv

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDependencies_OccupiedLockReadHasStableCode(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	writeLockfile(t, layout.UVLockFile())
	name, err := windows.UTF16PtrFromString(layout.UVLockFile())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	runner := &fakeDependenciesRunner{}
	service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Check(t.Context(), dependencyTestRequest(layout))
	assertPythonCode(t, err, protocol.CodeLockfileCheckFailed)
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("error = %v, want sharing violation in chain", err)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Stage() != protocol.StageDependenciesCheck || failure.Details()["failureKind"] != "lockfile_read" {
		t.Fatalf("failure = %#v, want lockfile_read at dependencies.check", failure)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %d, want 0 after read failure", len(runner.calls))
	}
}

func TestDependencies_LockReadPreservesCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "cancel", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatal(err)
			}
			writeLockfile(t, layout.UVLockFile())
			runner := &fakeDependenciesRunner{}
			service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if errors.Is(test.err, context.DeadlineExceeded) {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Time{})
			}
			cancel()
			err = service.checkLockfile(ctx, dependencyTestRequest(layout))
			var failure *Error
			if !errors.Is(err, test.err) || errors.As(err, &failure) {
				t.Fatalf("error = %v, want unwrapped %v", err, test.err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("runner calls = %d, want 0 after cancellation", len(runner.calls))
			}
		})
	}
}
