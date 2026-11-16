package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestDarwinRuntimeLogs_ConcurrentWritersAndActiveRemoval(t *testing.T) {
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
	var writers [2]*RuntimeLogWriter
	for index := range writers {
		writers[index], err = files.OpenAppend(t.Context(), "doctor", date)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := writers[index].Close(); err != nil {
				t.Error(err)
			}
		})
	}
	tokens, err := files.List(t.Context())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("List(active) = %v, %v, want one token", tokens, err)
	}
	if result, err := files.Remove(t.Context(), tokens[0]); err == nil || result.MutationApplied {
		t.Fatalf("Remove(active) = %#v, %v, want conflict", result, err)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range 40 {
		workers.Go(func() {
			<-start
			if n, err := writers[index%2].Write([]byte("record\n")); err != nil || n != 7 {
				t.Errorf("Write() = %d, %v, want 7, nil", n, err)
			}
		})
	}
	close(start)
	workers.Wait()
	data, err := os.ReadFile(writers[0].Path())
	if err != nil || string(data) != strings.Repeat("record\n", 40) {
		t.Fatalf("concurrent log = %q, %v, want 40 whole records", data, err)
	}
	for _, writer := range writers {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := files.Remove(t.Context(), tokens[0]); err != nil || !result.MutationApplied {
		t.Fatalf("Remove(after writers close) = %#v, %v, want removed", result, err)
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

func TestDarwinRuntimeLogs_Segments(t *testing.T) {
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
	date := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	first, err := files.OpenAppend(t.Context(), "doctor", date)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, pathErr := layout.RuntimeLogFile("doctor", date)
	if err := errors.Join(pathErr, first.Close()); err != nil {
		t.Fatal(err)
	}
	if first.Path() != firstPath {
		t.Fatalf("first segment = %q, want %q", first.Path(), firstPath)
	}

	writer, err := files.CreateSegment(t.Context(), "doctor", date, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := writer.Write([]byte("before\n"))
	if err := errors.Join(writeErr, writer.Close()); err != nil {
		t.Fatal(err)
	}
	next, err := files.OpenAppendSegment(t.Context(), "doctor", date, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr = next.Write([]byte("after\n"))
	if err := errors.Join(writeErr, next.Close()); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := files.CreateSegment(t.Context(), "doctor", date, 1); duplicate != nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("CreateSegment(existing) = %v, %v, want nil / os.ErrExist", duplicate, err)
	}
	for _, tc := range []struct {
		name     string
		sequence uint64
		link     func(string, string) error
	}{
		{name: "symbolic", sequence: 2, link: os.Symlink},
		{name: "hard", sequence: 3, link: os.Link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			path, err := layout.RuntimeLogSegmentFile("doctor", date, tc.sequence)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.link(sentinel, path); err != nil {
				t.Fatal(err)
			}
			if other, err := files.CreateSegment(t.Context(), "doctor", date, tc.sequence); other != nil || !errors.Is(err, os.ErrExist) {
				t.Fatalf("CreateSegment(link) = %v, %v, want nil / os.ErrExist", other, err)
			}
			if other, err := files.OpenAppendSegment(t.Context(), "doctor", date, tc.sequence); other != nil || err == nil {
				t.Fatalf("OpenAppendSegment(link) = %v, %v, want rejection", other, err)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "keep" {
				t.Fatalf("sentinel = %q, %v, want keep", data, err)
			}
		})
	}
	data, err := os.ReadFile(writer.Path())
	if err != nil || string(data) != "before\nafter\n" {
		t.Fatalf("segment content = %q, %v, want both records", data, err)
	}
}
