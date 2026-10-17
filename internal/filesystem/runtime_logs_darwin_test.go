package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinRuntimeLogs_WriterOutlivesDirectory(t *testing.T) {
	layout := darwinTestLayout(t)
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewRuntimeLogFiles() = %v, want nil", err)
	}
	writer, err := files.OpenAppend(t.Context(), "doctor", time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("record\n")); err != nil || n != 7 {
		t.Fatalf("Write after directory Close = %d, %v, want 7, nil", n, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write(closed) = %v, want ErrClosed", err)
	}
	data, err := os.ReadFile(writer.Path())
	if err != nil || string(data) != "record\n" {
		t.Fatalf("log contents = %q, %v, want record", data, err)
	}
}

func TestDarwinRuntimeLogs_CapacityAndLinks(t *testing.T) {
	layout := darwinTestLayout(t)
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	date := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	writer, err := files.OpenAppend(t.Context(), "doctor", date)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Truncate(writer.Path(), MaxRuntimeLogBytes-2); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("big")); n != 0 || !errors.Is(err, ErrRuntimeLogCapacity) {
		t.Fatalf("Write(over capacity) = %d, %v, want rejected whole record", n, err)
	}
	if err := os.Link(writer.Path(), filepath.Join(t.TempDir(), "external-link")); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("x")); n != 0 || !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("Write(hardlink) = %d, %v, want ErrUnsafeHardLink", n, err)
	}
	if other, err := files.OpenAppend(t.Context(), "doctor", date); err == nil || other != nil {
		t.Fatalf("OpenAppend(hardlink) = %v, %v, want rejection", other, err)
	}
}

func TestDarwinRuntimeLogs_ConditionalRemove(t *testing.T) {
	layout := darwinTestLayout(t)
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	writer, err := files.OpenAppend(t.Context(), "doctor", time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	listed, err := files.List(t.Context())
	if err != nil || len(listed) != 1 {
		t.Fatalf("List() = %v, %v, want one token", listed, err)
	}
	if err := os.Rename(writer.Path(), writer.Path()+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writer.Path(), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := files.Remove(t.Context(), listed[0])
	if !errors.Is(err, ErrIdentityChanged) || result.MutationApplied {
		t.Fatalf("Remove(replaced token) = %#v, %v, want identity rejection", result, err)
	}
	data, err := os.ReadFile(writer.Path())
	if err != nil || string(data) != "foreign" {
		t.Fatalf("replacement = %q, %v, want preserved", data, err)
	}
	fresh, err := files.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range fresh {
		if token.Path() == writer.Path() {
			result, err := files.Remove(t.Context(), token)
			if err != nil || !result.MutationApplied {
				t.Fatalf("Remove(current token) = %#v, %v, want removed", result, err)
			}
			return
		}
	}
	t.Fatal("List() omitted replacement ordinary file")
}
