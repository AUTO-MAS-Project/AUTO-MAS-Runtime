package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectExternalPath_ReadOnlyOrdinaryFacts(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.py")
	if err := os.WriteFile(file, []byte("pass\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	inspection, err := InspectExternalPath(t.Context(), root, true)
	if err != nil || !inspection.Exists {
		t.Fatalf("InspectExternalPath(dir) = %#v, %v, want existing directory", inspection, err)
	}
	inspection, err = InspectExternalPath(t.Context(), file, false)
	if err != nil || !inspection.Exists {
		t.Fatalf("InspectExternalPath(file) = %#v, %v, want existing file", inspection, err)
	}
	missing, err := InspectExternalPath(t.Context(), filepath.Join(root, "missing.py"), false)
	if err != nil || missing.Exists {
		t.Fatalf("InspectExternalPath(missing) = %#v, %v, want absent without mutation", missing, err)
	}
	if _, err := InspectExternalPath(t.Context(), file, true); !errors.Is(err, ErrExternalPathNotOrdinary) {
		t.Fatalf("InspectExternalPath(file as directory) error = %v, want ErrExternalPathNotOrdinary", err)
	}
	if _, err := InspectExternalPath(t.Context(), root, false); !errors.Is(err, ErrExternalPathNotOrdinary) {
		t.Fatalf("InspectExternalPath(dir as file) error = %v, want ErrExternalPathNotOrdinary", err)
	}
}

func TestInspectExternalPath_RejectsSymlinkWithoutCreatingFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := InspectExternalPath(t.Context(), link, true); !errors.Is(err, ErrExternalPathUnsafe) {
		t.Fatalf("InspectExternalPath(symlink) error = %v, want ErrExternalPathUnsafe", err)
	}
	if _, err := os.Stat(filepath.Join(link, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected file through symlink: %v", err)
	}
}

func TestPathContains_UsesCanonicalIdentity(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	inside, err := PathContains(t.Context(), root, child)
	if err != nil || !inside {
		t.Fatalf("PathContains(root, child) = %v, %v, want true", inside, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	inside, err = PathContains(t.Context(), root, outside)
	if err != nil || inside {
		t.Fatalf("PathContains(root, outside) = %v, %v, want false", inside, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PathContains(ctx, root, child); !errors.Is(err, context.Canceled) {
		t.Fatalf("PathContains(cancelled) error = %v, want context.Canceled", err)
	}
}
