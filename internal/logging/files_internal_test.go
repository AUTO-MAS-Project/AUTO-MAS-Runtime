package logging

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func newRealLogFiles(t *testing.T) (*filesystemLogFiles, string) {
	t.Helper()
	layout := mustTestLayout(t)
	if err := os.MkdirAll(layout.AppRoot(), 0o755); err != nil {
		t.Fatalf("MkdirAll(app root) error = %v", err)
	}
	files, err := newFilesystemLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("newFilesystemLogFiles() error = %v", err)
	}
	adapter, ok := files.(*filesystemLogFiles)
	if !ok {
		t.Fatalf("newFilesystemLogFiles() type = %T, want *filesystemLogFiles", files)
	}
	t.Cleanup(func() {
		if err := adapter.Close(); err != nil && !errors.Is(err, filesystem.ErrClosed) {
			t.Errorf("cleanup Close() error = %v", err)
		}
	})
	return adapter, layout.RuntimeLogDir()
}

func findRetainedByName(t *testing.T, files []retainedFile, name string) retainedFile {
	t.Helper()
	for _, file := range files {
		if file.Name() == name {
			return file
		}
	}
	t.Fatalf("retained file %q not found in %#v", name, files)
	return nil
}

func TestFilesystemLogFiles_ForwardsOpenListRemoveAndClose(t *testing.T) {
	files, _ := newRealLogFiles(t)
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	writer, err := files.OpenAppend(t.Context(), "doctor", now)
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("cleanup writer.Close() error = %v", err)
		}
	})
	if !filepath.IsAbs(writer.Path()) {
		t.Fatalf("writer.Path() = %q, want absolute", writer.Path())
	}
	if _, err := writer.Write([]byte("line\n")); err != nil {
		t.Fatalf("writer.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}

	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	token := findRetainedByName(t, listed, "doctor-20260729.log")
	result, err := files.Remove(t.Context(), token)
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if !result.mutationApplied {
		t.Fatal("Remove() MutationApplied = false, want true")
	}
	if _, err := os.Stat(token.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(removed) error = %v, want not exist", err)
	}

	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := files.List(t.Context()); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("List(after Close) error = %v, want filesystem.ErrClosed", err)
	}
}

func TestFilesystemLogFiles_TwoInstancesAppendAndDenyDeleteSharing(t *testing.T) {
	layout := mustTestLayout(t)
	if err := os.MkdirAll(layout.AppRoot(), 0o755); err != nil {
		t.Fatalf("MkdirAll(app root) error = %v", err)
	}
	first, err := newFilesystemLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("first newFilesystemLogFiles() error = %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil &&
			!errors.Is(err, filesystem.ErrClosed) {
			t.Errorf("cleanup first files Close() error = %v", err)
		}
	})
	second, err := newFilesystemLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("second newFilesystemLogFiles() error = %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil &&
			!errors.Is(err, filesystem.ErrClosed) {
			t.Errorf("cleanup second files Close() error = %v", err)
		}
	})

	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.Local)
	firstWriter, err := first.OpenAppend(t.Context(), "doctor", now)
	if err != nil {
		t.Fatalf("first OpenAppend() error = %v", err)
	}
	firstWriterOpen := true
	t.Cleanup(func() {
		if firstWriterOpen {
			if err := firstWriter.Close(); err != nil {
				t.Errorf("cleanup first writer Close() error = %v", err)
			}
		}
	})
	secondWriter, err := second.OpenAppend(t.Context(), "doctor", now)
	if err != nil {
		t.Fatalf(
			"second OpenAppend() error = %v, want concurrent append writer",
			err,
		)
	}
	secondWriterOpen := true
	t.Cleanup(func() {
		if secondWriterOpen {
			if err := secondWriter.Close(); err != nil {
				t.Errorf("cleanup second writer Close() error = %v", err)
			}
		}
	})
	if firstWriter.Path() != secondWriter.Path() {
		t.Fatalf(
			"writer paths = %q/%q, want same leaf",
			firstWriter.Path(),
			secondWriter.Path(),
		)
	}
	if _, err := firstWriter.Write([]byte("first\n")); err != nil {
		t.Fatalf("first writer Write() error = %v", err)
	}
	if _, err := secondWriter.Write([]byte("second\n")); err != nil {
		t.Fatalf("second writer Write() error = %v", err)
	}

	path := firstWriter.Path()
	assertRenameBlocked := func(stage string) {
		t.Helper()
		moved := path + ".sharing-check"
		if err := os.Rename(path, moved); err != nil {
			return
		}
		if err := os.Rename(moved, path); err != nil {
			t.Fatalf(
				"%s rename unexpectedly succeeded and restore failed: %v",
				stage,
				err,
			)
		}
		t.Fatalf("%s rename unexpectedly succeeded", stage)
	}
	assertRenameBlocked("two writers open")
	if err := firstWriter.Close(); err != nil {
		t.Fatalf("first writer Close() error = %v", err)
	}
	firstWriterOpen = false
	assertRenameBlocked("second writer still open")
	if err := secondWriter.Close(); err != nil {
		t.Fatalf("second writer Close() error = %v", err)
	}
	secondWriterOpen = false

	moved := path + ".released-check"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("Rename(after both writers Close) error = %v", err)
	}
	if err := os.Rename(moved, path); err != nil {
		t.Fatalf("restore Rename() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(shared append leaf) error = %v", err)
	}
	if got, want := string(content), "first\nsecond\n"; got != want {
		t.Fatalf("shared append bytes = %q, want %q", got, want)
	}
}

func TestFilesystemLogFiles_ReturnsWriterPathAndOriginalToken(t *testing.T) {
	files, _ := newRealLogFiles(t)
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local)
	writer, err := files.OpenAppend(t.Context(), "workspace-sync", now)
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("cleanup writer.Close() error = %v", err)
		}
	})
	path := writer.Path()
	if _, err := writer.Write([]byte("original\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	original := findRetainedByName(t, listed, filepath.Base(path))

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove original fixture: %v", err)
	}
	replacement := []byte("replacement must survive\n")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("create replacement fixture: %v", err)
	}
	result, err := files.Remove(t.Context(), original)
	if result.mutationApplied {
		t.Fatal("Remove(old token) MutationApplied = true, want false")
	}
	if !errors.Is(err, filesystem.ErrIdentityChanged) {
		t.Fatalf("Remove(old token) error = %v, want filesystem.ErrIdentityChanged", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(replacement) error = %v", readErr)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement bytes = %q, want %q", got, replacement)
	}
}

func TestFilesystemLogFiles_PreservesMutationAppliedAndFilesystemErrorChains(t *testing.T) {
	files, _ := newRealLogFiles(t)
	_, err := files.Remove(t.Context(), fakeRetainedFile{name: "forged.log", path: "forged.log"})
	if !errors.Is(err, filesystem.ErrInvalidToken) {
		t.Fatalf("Remove(forged) error = %v, want filesystem.ErrInvalidToken", err)
	}

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	writer, err := files.OpenAppend(t.Context(), "doctor", now)
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("cleanup writer.Close() error = %v", err)
		}
	})
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	result, err := files.Remove(t.Context(), findRetainedByName(t, listed, "doctor-20260701.log"))
	if err != nil || !result.mutationApplied {
		t.Fatalf("Remove() = (%#v, %v), want applied without error", result, err)
	}
}
