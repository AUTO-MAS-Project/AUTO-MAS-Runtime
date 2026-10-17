package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func darwinTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestDarwinDirectory_PrepareAndInspect(t *testing.T) {
	layout := darwinTestLayout(t)
	path := filepath.Join(layout.AppRoot(), "managed")
	lease, err := PrepareManagedDirectory(t.Context(), layout, path)
	if err != nil {
		t.Fatalf("PrepareManagedDirectory() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	inspection, err := InspectManagedDirectory(t.Context(), layout, path)
	if err != nil || !inspection.Exists || !lease.Identity().Equal(inspection.Identity) {
		t.Fatalf("InspectManagedDirectory() = %#v, %v, want same existing identity", inspection, err)
	}
	if existing, err := PrepareManagedDirectory(t.Context(), layout, path); err == nil || existing != nil {
		t.Fatalf("PrepareManagedDirectory(existing) = %v, %v, want rejection", existing, err)
	}
	missing, err := InspectManagedDirectory(t.Context(), layout, filepath.Join(path, "missing", "child"))
	if err != nil || missing.Exists {
		t.Fatalf("InspectManagedDirectory(missing tail) = %#v, %v, want absent", missing, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close twice() = %v, want nil", err)
	}
}

func TestDarwinDirectory_RejectsUnsafePaths(t *testing.T) {
	layout := darwinTestLayout(t)
	outside := t.TempDir()
	inside := filepath.Join(layout.AppRoot(), "ordinary")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, target string }{
		{name: "outside link", target: outside},
		{name: "inside link", target: inside},
	} {
		t.Run(test.name, func(t *testing.T) {
			link := filepath.Join(layout.AppRoot(), test.name)
			if err := os.Symlink(test.target, link); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{link, filepath.Join(link, "missing")} {
				if lease, err := OpenManagedDirectory(t.Context(), layout, path); err == nil || lease != nil {
					t.Fatalf("OpenManagedDirectory(%q) = %v, %v, want rejection", path, lease, err)
				}
				if lease, err := EnsureManagedDirectory(t.Context(), layout, path); err == nil || lease != nil {
					t.Fatalf("EnsureManagedDirectory(%q) = %v, %v, want rejection", path, lease, err)
				}
			}
		})
	}
	for _, path := range []string{outside, layout.AppRoot(), filepath.Join(layout.AppRoot(), "..", "escape")} {
		if lease, err := PrepareManagedDirectory(t.Context(), layout, path); err == nil || lease != nil {
			t.Fatalf("PrepareManagedDirectory(%q) = %v, %v, want rejection", path, lease, err)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside entries = %v, %v, want empty", entries, err)
	}
}

func TestDarwinFile_RejectsLinksAndSpecialFiles(t *testing.T) {
	layout := darwinTestLayout(t)
	ordinary := filepath.Join(layout.AppRoot(), "ordinary")
	if err := os.WriteFile(ordinary, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectManagedFile(t.Context(), layout, ordinary)
	if err != nil || !inspection.Exists {
		t.Fatalf("InspectManagedFile(ordinary) = %#v, %v, want present", inspection, err)
	}
	link := filepath.Join(layout.AppRoot(), "link")
	if err := os.Symlink(ordinary, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectManagedFile(t.Context(), layout, link); err == nil {
		t.Fatal("InspectManagedFile(symlink) error = nil, want rejection")
	}
	hardlink := filepath.Join(layout.AppRoot(), "hardlink")
	if err := os.Link(ordinary, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectManagedFile(t.Context(), layout, ordinary); !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("InspectManagedFile(hardlink) error = %v, want ErrUnsafeHardLink", err)
	}
	if _, err := InspectManagedFile(t.Context(), layout, layout.AppRoot()); err == nil {
		t.Fatal("InspectManagedFile(directory) error = nil, want rejection")
	}
	fifo := filepath.Join(layout.AppRoot(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectManagedFile(t.Context(), layout, fifo); err == nil {
		t.Fatal("InspectManagedFile(fifo) error = nil, want rejection without blocking")
	}
}

func TestDarwinDirectory_RejectsRootAliasAndOtherVolume(t *testing.T) {
	layout := darwinTestLayout(t)
	link := filepath.Join(t.TempDir(), "app-link")
	if err := os.Symlink(layout.AppRoot(), link); err != nil {
		t.Fatal(err)
	}
	linkedLayout, err := config.NewLayout(link, filepath.Dir(link))
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := OpenManagedDirectory(t.Context(), linkedLayout, link); err == nil || lease != nil {
		t.Fatalf("OpenManagedDirectory(link root) = %v, %v, want rejection", lease, err)
	}
	otherLayout, err := config.NewLayout("/dev", "/")
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := OpenManagedDirectory(t.Context(), otherLayout, "/dev"); err == nil || lease != nil {
		t.Fatalf("OpenManagedDirectory(devfs) = %v, %v, want rejection", lease, err)
	}
}

func TestDarwinDirectory_CancellationAndIdentity(t *testing.T) {
	layout := darwinTestLayout(t)
	path := filepath.Join(layout.AppRoot(), "managed")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if lease, err := PrepareManagedDirectory(ctx, layout, path); !errors.Is(err, context.Canceled) || lease != nil {
		t.Fatalf("PrepareManagedDirectory(canceled) = %v, %v, want context.Canceled", lease, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(canceled path) = %v, want absent", err)
	}
	lease, err := PrepareManagedDirectory(t.Context(), layout, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	inspection, inspectErr := InspectManagedDirectory(t.Context(), layout, path)
	closeErr := lease.Close()
	if inspectErr != nil || closeErr != nil || !inspection.Exists || lease.Identity().Equal(inspection.Identity) {
		t.Fatalf("replacement identity = %#v, %v; Close = %v, want different object", inspection, inspectErr, closeErr)
	}
}
