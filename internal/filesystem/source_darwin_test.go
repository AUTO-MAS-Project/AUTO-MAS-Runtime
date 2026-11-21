//go:build darwin

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestDarwinReadManagedFile_Boundaries(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.RepoDir(), "uv.lock")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadManagedFile(t.Context(), layout, path, 3)
	if err != nil || string(data) != "abc" {
		t.Fatalf("ReadManagedFile() = %q, %v, want abc", data, err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManagedFile(t.Context(), layout, outside, 10); err == nil {
		t.Fatal("outside path accepted")
	}
	if _, err := ReadManagedFile(t.Context(), layout, layout.RepoDir()+"/../repo/uv.lock", 10); err == nil {
		t.Fatal("parent traversal accepted")
	}
	link := filepath.Join(layout.RepoDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManagedFile(t.Context(), layout, link, 10); err == nil {
		t.Fatal("symlink accepted")
	}
	dirLink := filepath.Join(layout.RepoDir(), "dirlink")
	if err := os.Symlink(root, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManagedFile(t.Context(), layout, filepath.Join(dirLink, "outside"), 10); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	hard := filepath.Join(layout.RepoDir(), "hard")
	if err := os.Link(outside, hard); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManagedFile(t.Context(), layout, hard, 10); !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("hardlink error = %v, want ErrUnsafeHardLink", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadManagedFile(ctx, layout, path, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if data, err := ReadManagedFile(t.Context(), layout, path, 0); err != nil || len(data) != 0 {
		t.Fatalf("zero limit = %q, %v, want empty", data, err)
	}
	missing := filepath.Join(layout.RepoDir(), "missing", "uv.lock")
	if _, err := ReadManagedFile(t.Context(), layout, missing, 10); err == nil {
		t.Fatal("missing path accepted")
	}
	if _, err := os.Lstat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent stat = %v, want not exist", err)
	}
}
