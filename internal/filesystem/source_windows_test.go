//go:build windows

package filesystem

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestManagedSourceFile_ReadAndReplace(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.RepoDir(), "app", "source.py")
	if err := ReplaceManagedFile(t.Context(), layout, path, []byte("original\n")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManagedFile(t.Context(), layout, path, 100)
	if err != nil || string(got) != "original\n" {
		t.Fatalf("ReadManagedFile() = %q, %v, want original", got, err)
	}
	if err := ReplaceManagedFile(t.Context(), layout, path, []byte("restored\n")); err != nil {
		t.Fatal(err)
	}
	got, err = ReadManagedFile(t.Context(), layout, path, 4)
	if err != nil || string(got) != "rest" {
		t.Fatalf("bounded read = %q, %v, want rest", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ReplaceManagedFile(ctx, layout, path, []byte("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "restored\n" {
		t.Fatalf("cancelled replacement = %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("parent entries = %v, %v, want one", entries, err)
	}
}

func TestManagedSourceFile_RejectsReparse(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	path := filepath.Join(layout.RepoDir(), "escape")
	cmd := exec.CommandContext(t.Context(), "pwsh", "-NoProfile", "-NonInteractive", "-Command", "New-Item -ItemType Junction -Path $env:SOURCE_TEST_LINK -Target $env:SOURCE_TEST_TARGET | Out-Null")
	cmd.Env = append(os.Environ(), "SOURCE_TEST_LINK="+path, "SOURCE_TEST_TARGET="+outside)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v, %s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
	})
	if err := ReplaceManagedFile(t.Context(), layout, filepath.Join(path, "sentinel"), []byte("bad")); err == nil {
		t.Fatal("ReplaceManagedFile() accepted reparse ancestor")
	}
	if _, err := ReadManagedFile(t.Context(), layout, filepath.Join(path, "sentinel"), 10); err == nil {
		t.Fatal("ReadManagedFile() accepted reparse ancestor")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside entries = %v, %v, want empty", entries, err)
	}
}

func TestManagedSourceFile_OccupiedTargetStaysIntact(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	path := layout.UVLockFile()
	if err := ReplaceManagedFile(t.Context(), layout, path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceManagedFile(t.Context(), layout, path, []byte("replaced")); err == nil {
		t.Error("occupied replacement unexpectedly succeeded")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "original" {
		t.Fatalf("occupied source = %q, %v", got, err)
	}
	entries, err := os.ReadDir(layout.RepoDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
}
