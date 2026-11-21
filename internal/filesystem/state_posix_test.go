//go:build linux || darwin

package filesystem_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestPOSIXState_AtomicRoundTrip(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filesystem.NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	payload := []byte(`{"version":1}`)
	result, err := files.WriteAtomic(t.Context(), filesystem.StateMutation, payload)
	if err != nil || !result.MutationApplied {
		t.Fatalf("got %+v, %v, want applied", result, err)
	}
	snapshot, err := files.Read(t.Context(), filesystem.StateMutation, 1024)
	if err != nil || !bytes.Equal(snapshot.Bytes(), payload) {
		t.Fatalf("got %q, %v", snapshot.Bytes(), err)
	}
	if _, err = files.WriteAtomic(t.Context(), filesystem.StateMutation, []byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = files.RemoveTransactionIfUnchanged(t.Context(), snapshot); !errors.Is(err, filesystem.ErrIdentityChanged) {
		t.Fatalf("got %v, want identity changed", err)
	}
	snapshot, err = files.Read(t.Context(), filesystem.StateMutation, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = files.RemoveTransactionIfUnchanged(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = files.Read(t.Context(), filesystem.StateMutation, 1024); !errors.Is(err, filesystem.ErrStateFileNotFound) {
		t.Fatalf("got %v, want missing", err)
	}
}
