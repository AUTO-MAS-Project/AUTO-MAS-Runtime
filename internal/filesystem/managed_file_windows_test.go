//go:build windows

package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestInspectManagedFile_RequiresOrdinaryManagedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	missing, err := InspectManagedFile(t.Context(), layout, layout.BackendEntryFile())
	if err != nil {
		t.Fatalf("InspectManagedFile(missing) error = %v", err)
	}
	if missing.Exists {
		t.Fatal("InspectManagedFile(missing).Exists = true, want false")
	}

	if err := os.WriteFile(layout.BackendEntryFile(), []byte("print('ready')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectManagedFile(t.Context(), layout, layout.BackendEntryFile())
	if err != nil {
		t.Fatalf("InspectManagedFile(file) error = %v", err)
	}
	if !inspection.Exists {
		t.Fatal("InspectManagedFile(file).Exists = false, want true")
	}

	if err := os.Remove(layout.BackendEntryFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(layout.BackendEntryFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectManagedFile(t.Context(), layout, layout.BackendEntryFile()); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("InspectManagedFile(directory) error = %v, want ErrIdentityChanged", err)
	}
}

func TestInspectManagedFile_RejectsReparsePoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.py")
	if err := os.WriteFile(external, []byte("print('unsafe')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, layout.BackendEntryFile()); err != nil {
		t.Skipf("creating file symlink is unavailable: %v", err)
	}

	_, err = InspectManagedFile(t.Context(), layout, layout.BackendEntryFile())
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) || coded.Code() != protocol.CodeUnsafeReparsePoint {
		t.Fatalf("InspectManagedFile(reparse) error = %v, want UNSAFE_REPARSE_POINT", err)
	}
}
