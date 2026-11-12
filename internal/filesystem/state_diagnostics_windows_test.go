//go:build windows

package filesystem

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStateFiles_FailureUnlinkMode(t *testing.T) {
	for _, mode := range []string{"posix", "classic", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			files, _ := newStateFilesFixture(t)
			originalAPI := files.api
			if mode != "unknown" {
				if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("prime")); err != nil {
					t.Fatal(err)
				}
				files.classicUnlink = mode == "classic"
				files.api.setStateDisposition = func(windows.Handle, stateDispositionSpec) error { return windows.ERROR_ACCESS_DENIED }
			} else {
				files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
					return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
				}
			}
			_, err := files.WriteAtomic(t.Context(), StateMutation, []byte("new"))
			var writeErr *StateWriteError
			if !errors.As(err, &writeErr) {
				t.Fatalf("got %v, want StateWriteError", err)
			}
			if writeErr.UnlinkMode != mode {
				t.Fatalf("got mode %v, want %s", writeErr.UnlinkMode, mode)
			}
			files.api = originalAPI
			if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("recovered")); err != nil {
				t.Fatalf("recover fixture state: %v", err)
			}
		})
	}
}

func TestStateFiles_RemoveFailureUnlinkMode(t *testing.T) {
	for _, mode := range []string{"posix", "classic"} {
		t.Run(mode, func(t *testing.T) {
			files, _ := newStateFilesFixture(t)
			snapshot := writeAndReadStateSnapshot(t, files, StateMutation, []byte("payload"))
			originalAPI := files.api
			files.classicUnlink = mode == "classic"
			files.api.setStateDisposition = func(windows.Handle, stateDispositionSpec) error { return windows.ERROR_ACCESS_DENIED }
			_, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
			var failure *StateRemoveError
			if !errors.As(err, &failure) || failure.UnlinkMode != mode {
				t.Fatalf("got %#v, want %s removal failure", err, mode)
			}
			files.api = originalAPI
			if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("recovered")); err != nil {
				t.Fatalf("recover fixture state: %v", err)
			}
		})
	}
}
