//go:build windows

package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func newRealStore(
	t *testing.T,
	layout *config.Layout,
) *Store {
	t.Helper()
	store, err := NewStore(t.Context(), layout, WithClock(fixedStateTime))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return store
}

func newRealLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func TestStore_WriteReturnsLogicalWholeValues(t *testing.T) {
	t.Parallel()

	layout := newRealLayout(t)
	writer := newRealStore(t, layout)
	reader := newRealStore(t, layout)
	oldValue := validTransactionState(TransactionMutation)
	oldValue.TargetVersion = "v5.4.0-old"
	newValue := oldValue
	newValue.TargetVersion = "v5.4.0-new"

	if err := writer.WriteTransaction(
		t.Context(),
		TransactionMutation,
		oldValue,
	); err != nil {
		t.Fatalf("write old error = %v", err)
	}
	oldSnapshot, err := reader.ReadTransaction(
		t.Context(),
		TransactionMutation,
	)
	if err != nil || oldSnapshot.State() != oldValue {
		t.Fatalf("read old = %#v, %v", oldSnapshot.State(), err)
	}

	stop := make(chan struct{})
	stopReader := sync.OnceFunc(func() {
		close(stop)
	})
	t.Cleanup(stopReader)
	readErr := make(chan error, 1)
	readerReady := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		firstRead := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			snapshot, readError := reader.ReadTransaction(
				t.Context(),
				TransactionMutation,
			)
			if readError != nil {
				select {
				case readErr <- readError:
				default:
				}
				return
			}
			got := snapshot.State()
			if got != oldValue && got != newValue {
				select {
				case readErr <- errors.New("reader observed mixed transaction"):
				default:
				}
				return
			}
			if firstRead {
				close(readerReady)
				firstRead = false
			}
		}
	}()
	waitForTestSignal(t, readerReady, "atomic reader first old value")
	if err := writer.WriteTransaction(
		t.Context(),
		TransactionMutation,
		newValue,
	); err != nil {
		stopReader()
		waitForTestSignal(t, readerDone, "atomic reader failure")
		t.Fatalf("write new error = %v", err)
	}
	stopReader()
	waitForTestSignal(t, readerDone, "atomic reader completion")
	select {
	case err := <-readErr:
		t.Fatalf("concurrent reader error = %v", err)
	default:
	}
	newSnapshot, err := reader.ReadTransaction(
		t.Context(),
		TransactionMutation,
	)
	if err != nil || newSnapshot.State() != newValue {
		t.Fatalf("read new = %#v, %v", newSnapshot.State(), err)
	}
}

func TestStore_WriteDoesNotGuessOrphanSidecars(t *testing.T) {
	t.Parallel()

	layout := newRealLayout(t)
	store := newRealStore(t, layout)
	orphanPayload := []byte(`{"schemaVersion":1`)
	orphanPaths := []string{
		filepath.Join(layout.StateDir(), "backend.crash.tmp"),
		filepath.Join(layout.StateDir(), "backend.intent-stage.tmp"),
	}
	for _, orphanPath := range orphanPaths {
		if err := os.WriteFile(orphanPath, orphanPayload, 0o600); err != nil {
			t.Fatalf("os.WriteFile(orphan) error = %v", err)
		}
	}
	if _, err := store.ReadTransaction(
		t.Context(),
		TransactionBackend,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadTransaction(temp only) error = %v, want ErrNotFound", err)
	}

	want := validTransactionState(TransactionBackend)
	if err := store.WriteTransaction(
		t.Context(),
		TransactionBackend,
		want,
	); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(layout.StateDir(), "backend.other.tmp"),
		[]byte("{"),
		0o600,
	); err != nil {
		t.Fatalf("os.WriteFile(other temp) error = %v", err)
	}
	snapshot, err := store.ReadTransaction(t.Context(), TransactionBackend)
	if err != nil || snapshot.State() != want {
		t.Fatalf("ReadTransaction() = %#v, %v, want target", snapshot.State(), err)
	}
	for _, orphanPath := range append(
		orphanPaths,
		filepath.Join(layout.StateDir(), "backend.other.tmp"),
	) {
		got, err := os.ReadFile(orphanPath)
		if err != nil {
			t.Fatalf("os.ReadFile(orphan) error = %v", err)
		}
		if !bytes.Equal(got, orphanPayload) && string(got) != "{" {
			t.Fatalf("orphan bytes = %q, want original bytes", got)
		}
	}
}

func TestStore_RemoveTransactionRejectsRealReplacementAndRewrite(t *testing.T) {
	t.Parallel()

	t.Run("replacement", func(t *testing.T) {
		layout := newRealLayout(t)
		first := newRealStore(t, layout)
		second := newRealStore(t, layout)
		oldValue := validTransactionState(TransactionUpdate)
		oldValue.TargetVersion = "v5.4.0-old"
		newValue := oldValue
		newValue.TargetVersion = "v5.4.0-new"
		if err := first.WriteTransaction(
			t.Context(),
			TransactionUpdate,
			oldValue,
		); err != nil {
			t.Fatalf("write old error = %v", err)
		}
		snapshot, err := first.ReadTransaction(
			t.Context(),
			TransactionUpdate,
		)
		if err != nil {
			t.Fatalf("read old error = %v", err)
		}
		if err := second.WriteTransaction(
			t.Context(),
			TransactionUpdate,
			newValue,
		); err != nil {
			t.Fatalf("write replacement error = %v", err)
		}
		if err := first.RemoveTransaction(
			t.Context(),
			snapshot,
		); !errors.Is(err, ErrTransactionChanged) {
			t.Fatalf("RemoveTransaction() error = %v, want changed", err)
		}
		current, err := first.ReadTransaction(t.Context(), TransactionUpdate)
		if err != nil || current.State() != newValue {
			t.Fatalf("current = %#v, %v, want replacement", current.State(), err)
		}
	})

	t.Run("same_file_rewrite", func(t *testing.T) {
		layout := newRealLayout(t)
		store := newRealStore(t, layout)
		oldValue := validTransactionState(TransactionBackend)
		newValue := oldValue
		newValue.TargetVersion = "v5.4.0-rewritten"
		if err := store.WriteTransaction(
			t.Context(),
			TransactionBackend,
			oldValue,
		); err != nil {
			t.Fatalf("write old error = %v", err)
		}
		snapshot, err := store.ReadTransaction(
			t.Context(),
			TransactionBackend,
		)
		if err != nil {
			t.Fatalf("read old error = %v", err)
		}
		payload := mustStateJSON(t, newValue)
		file, err := os.OpenFile(
			layout.BackendStateFile(),
			os.O_WRONLY|os.O_TRUNC,
			0,
		)
		if err != nil {
			t.Fatalf("os.OpenFile(rewrite) error = %v", err)
		}
		if _, err := file.Write(payload); err != nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Errorf("rewrite Close() after Write error = %v", closeErr)
			}
			t.Fatalf("rewrite Write() error = %v", err)
		}
		if err := file.Sync(); err != nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Errorf("rewrite Close() after Sync error = %v", closeErr)
			}
			t.Fatalf("rewrite Sync() error = %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("rewrite Close() error = %v", err)
		}
		if err := store.RemoveTransaction(
			t.Context(),
			snapshot,
		); !errors.Is(err, ErrTransactionChanged) {
			t.Fatalf("RemoveTransaction() error = %v, want changed", err)
		}
		current, err := store.ReadTransaction(t.Context(), TransactionBackend)
		if err != nil || current.State() != newValue {
			t.Fatalf("current = %#v, %v, want rewritten", current.State(), err)
		}
	})
}

func TestStore_RemoveOwnedTransactionRejectsDifferentOperation(t *testing.T) {
	layout := newRealLayout(t)
	store := newRealStore(t, layout)
	current := validTransactionState(TransactionMutation)
	if err := store.WriteTransaction(t.Context(), TransactionMutation, current); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	expected := current
	expected.OperationID = "01J00000000000000000000009"

	if err := store.RemoveOwnedTransaction(
		t.Context(),
		TransactionMutation,
		expected,
	); !errors.Is(err, ErrTransactionChanged) {
		t.Fatalf("RemoveOwnedTransaction(other operation) error = %v, want ErrTransactionChanged", err)
	}
	snapshot, err := store.ReadTransaction(t.Context(), TransactionMutation)
	if err != nil || snapshot.State() != current {
		t.Fatalf("current transaction = %#v, %v, want retained %#v", snapshot.State(), err, current)
	}
	if err := store.RemoveOwnedTransaction(
		t.Context(),
		TransactionMutation,
		current,
	); err != nil {
		t.Fatalf("RemoveOwnedTransaction(current) error = %v", err)
	}
	if _, err := store.ReadTransaction(t.Context(), TransactionMutation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadTransaction() error = %v, want ErrNotFound", err)
	}
}

func TestStore_StateDirectoryReparsePointFailsClosed(t *testing.T) {
	t.Parallel()

	layout := newRealLayout(t)
	external := t.TempDir()
	markerPath := filepath.Join(external, "marker.txt")
	marker := []byte("external-must-remain")
	if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
		t.Fatalf("os.WriteFile(marker) error = %v", err)
	}
	if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(app root) error = %v", err)
	}
	if err := os.Symlink(external, layout.StateDir()); err != nil {
		t.Skipf("create Windows directory symlink: %v", err)
	}
	if _, err := NewStore(
		t.Context(),
		layout,
		WithClock(fixedStateTime),
	); err == nil {
		t.Fatal("NewStore(reparse state dir) error = nil, want failure")
	}
	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("os.ReadFile(marker) error = %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Fatalf("external marker = %q, want %q", got, marker)
	}
}

func TestStore_CloseReleasesRealStateHandles(t *testing.T) {
	t.Parallel()

	layout := newRealLayout(t)
	store, err := NewStore(
		t.Context(),
		layout,
		WithClock(fixedStateTime),
	)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := store.WriteTransaction(
		t.Context(),
		TransactionBackend,
		validTransactionState(TransactionBackend),
	); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	renamed := layout.StateDir() + ".closed"
	if err := os.Rename(layout.StateDir(), renamed); err != nil {
		t.Fatalf("os.Rename(state dir after Close) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Store.Close() error = %v", err)
	}
}

func TestStore_RealStateFilesRoundTripAllKinds(t *testing.T) {
	t.Parallel()

	layout := newRealLayout(t)
	store := newRealStore(t, layout)
	for _, kind := range []TransactionKind{
		TransactionBackend,
		TransactionMutation,
		TransactionUpdate,
	} {
		want := validTransactionState(kind)
		if err := store.WriteTransaction(t.Context(), kind, want); err != nil {
			t.Fatalf("WriteTransaction(%q) error = %v", kind, err)
		}
		snapshot, err := store.ReadTransaction(t.Context(), kind)
		if err != nil || snapshot.State() != want {
			t.Fatalf(
				"ReadTransaction(%q) = %#v, %v",
				kind,
				snapshot.State(),
				err,
			)
		}
	}
	environment, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := store.WriteEnvironment(t.Context(), environment); err != nil {
		t.Fatalf("WriteEnvironment() error = %v", err)
	}
	got, err := store.ReadEnvironment(t.Context())
	if err != nil || got != environment {
		t.Fatalf("ReadEnvironment() = %#v, %v", got, err)
	}
}
