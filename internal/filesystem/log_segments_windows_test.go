package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRuntimeLogFiles_CreateSegmentSafety(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	now := time.Now()
	writer, err := files.CreateSegment(t.Context(), "doctor", now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("preserved\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := files.CreateSegment(t.Context(), "doctor", now, 1); !errors.Is(err, fs.ErrExist) || duplicate != nil {
		t.Fatalf("CreateSegment(existing) = %v, %v, want nil/fs.ErrExist", duplicate, err)
	}
	content, err := os.ReadFile(writer.Path())
	if err != nil || string(content) != "preserved\n" {
		t.Fatalf("ReadFile() = %q, %v, want preserved content", content, err)
	}
	hardLink, err := layout.RuntimeLogSegmentFile("doctor", now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(writer.Path(), hardLink); err != nil {
		t.Fatal(err)
	}
	if opened, err := files.OpenAppendSegment(t.Context(), "doctor", now, 2); !errors.Is(err, ErrUnsafeHardLink) || opened != nil {
		t.Fatalf("OpenAppendSegment(hardlink) = %v, %v, want nil/ErrUnsafeHardLink", opened, err)
	}
}

func TestRuntimeLogFiles_CreateSegmentDoesNotProbeExistingLeaf(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	now := time.Now()
	writer, err := files.CreateSegment(t.Context(), "doctor", now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	probes := 0
	files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		probes++
		return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
	}
	if _, err := files.CreateSegment(t.Context(), "doctor", now, 1); !errors.Is(err, fs.ErrExist) || probes != 0 {
		t.Fatalf("CreateSegment(existing) = %v, probes %d, want fs.ErrExist without classification", err, probes)
	}
}
