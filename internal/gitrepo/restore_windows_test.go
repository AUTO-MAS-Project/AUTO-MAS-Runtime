//go:build windows

package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestService_RestoreFailureKeepsBrokenState(t *testing.T) {
	layout, commit := sourceRestoreFixture(t)
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.NewReadyEnvironment("v1.0.0", commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(layout.ConfigDir(), "user.json")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(layout.UVLockFile())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != windows.InvalidHandle {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}
	}()
	service, err := NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	request := componentServiceSyncRequest(layout, mustServiceTarget(t, "v1.0.0"), policy, componentOperationID(42), &recordingServiceEmitter{})
	request.UseCurrent = true
	request.PID = uint32(os.Getpid())
	_, err = service.Sync(t.Context(), request)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code() != protocol.CodeWorkspaceRestoreFailed || !failure.Committed() {
		t.Fatalf("occupied Sync error = %v, want committed restore failure", err)
	}
	store, err = state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.ReadEnvironment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if environment.Status != protocol.StateEnvironmentBroken || environment.Broken.Reason != state.ReasonRepositoryChanged {
		t.Fatalf("environment after failure = %#v", environment)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	result, err := service.Sync(t.Context(), request)
	if err != nil || !result.Changed || result.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("resumed Sync = %#v, %v", result, err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "keep" {
		t.Fatalf("user data = %q, %v", got, err)
	}
}
