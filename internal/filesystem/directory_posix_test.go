//go:build linux || darwin

package filesystem_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestPOSIXDirectory_RejectSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "linked")
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if lease, err := filesystem.OpenManagedDirectory(t.Context(), layout, path); err == nil {
		_ = lease.Close()
		t.Fatal("got successful symlink lease, want rejection")
	}
	lease, err := filesystem.PrepareManagedDirectory(t.Context(), layout, layout.RuntimeDir())
	if err != nil {
		t.Fatalf("prepare ordinary directory: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}
