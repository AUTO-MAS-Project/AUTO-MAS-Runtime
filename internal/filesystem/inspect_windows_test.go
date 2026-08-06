package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestInspectManagedDirectory_MissingPathsRemainAbsent(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	layout := inspectTestLayout(t, appRoot, root)
	api := newProductionPathAPI()
	api.makeDirectory = func(string) error {
		t.Fatal("InspectManagedDirectory created a directory")
		return nil
	}

	inspection, err := inspectManagedDirectoryWith(t.Context(), layout, layout.AppRoot(), api)
	if err != nil || inspection.Exists {
		t.Fatalf("InspectManagedDirectory(app-root) = %#v, %v, want missing", inspection, err)
	}
	if _, err := os.Lstat(appRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app-root was created: %v", err)
	}

	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	missingTail := filepath.Join(layout.RepoDir(), "future", "leaf")
	inspection, err = inspectManagedDirectoryWith(t.Context(), layout, missingTail, api)
	if err != nil || inspection.Exists {
		t.Fatalf("InspectManagedDirectory(missing tail) = %#v, %v, want missing", inspection, err)
	}
	if _, err := os.Lstat(layout.RepoDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing tail parent was created: %v", err)
	}
}

func TestInspectManagedDirectory_ExistingDirectoryAndFileType(t *testing.T) {
	root := t.TempDir()
	layout := inspectTestLayout(t, filepath.Join(root, "app"), root)
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	inspection, err := InspectManagedDirectory(t.Context(), layout, layout.RepoDir())
	if err != nil || !inspection.Exists {
		t.Fatalf("InspectManagedDirectory(directory) = %#v, %v, want existing", inspection, err)
	}

	filePath := filepath.Join(layout.AppRoot(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	inspection, err = InspectManagedDirectory(t.Context(), layout, filePath)
	if inspection != (DirectoryInspection{}) || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("InspectManagedDirectory(file) = %#v, %v, want identity rejection", inspection, err)
	}
}

func TestInspectManagedDirectory_RejectsDirectAndAncestorReparsePoints(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatalf("MkdirAll(external) error = %v", err)
	}

	t.Run("direct", func(t *testing.T) {
		appRoot := filepath.Join(root, "direct-app")
		layout := inspectTestLayout(t, appRoot, root)
		if err := os.MkdirAll(appRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(app-root) error = %v", err)
		}
		if err := os.Symlink(external, layout.RepoDir()); err != nil {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		inspection, err := InspectManagedDirectory(t.Context(), layout, layout.RepoDir())
		if inspection != (DirectoryInspection{}) {
			t.Fatalf("inspection = %#v, want zero", inspection)
		}
		assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	})

	t.Run("ancestor", func(t *testing.T) {
		realParent := filepath.Join(root, "real-parent")
		if err := os.MkdirAll(filepath.Join(realParent, "app"), 0o700); err != nil {
			t.Fatalf("MkdirAll(real app) error = %v", err)
		}
		alias := filepath.Join(root, "ancestor-alias")
		if err := os.Symlink(realParent, alias); err != nil {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		layout := inspectTestLayout(t, filepath.Join(alias, "app"), root)
		inspection, err := InspectManagedDirectory(t.Context(), layout, layout.AppRoot())
		if inspection != (DirectoryInspection{}) {
			t.Fatalf("inspection = %#v, want zero", inspection)
		}
		assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	})
}

func TestInspectManagedDirectory_RejectsDanglingReparsePoints(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-target")

	t.Run("direct", func(t *testing.T) {
		appRoot := filepath.Join(root, "dangling-direct-app")
		layout := inspectTestLayout(t, appRoot, root)
		if err := os.MkdirAll(appRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(app-root) error = %v", err)
		}
		if err := os.Symlink(missing, layout.RepoDir()); err != nil {
			t.Skipf("dangling directory symlink unavailable: %v", err)
		}
		inspection, err := InspectManagedDirectory(t.Context(), layout, layout.RepoDir())
		if inspection != (DirectoryInspection{}) {
			t.Fatalf("inspection = %#v, want zero", inspection)
		}
		assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	})

	t.Run("ancestor", func(t *testing.T) {
		alias := filepath.Join(root, "dangling-ancestor")
		if err := os.Symlink(missing, alias); err != nil {
			t.Skipf("dangling ancestor symlink unavailable: %v", err)
		}
		layout := inspectTestLayout(t, filepath.Join(alias, "app"), root)
		inspection, err := InspectManagedDirectory(t.Context(), layout, layout.AppRoot())
		if inspection != (DirectoryInspection{}) {
			t.Fatalf("inspection = %#v, want zero", inspection)
		}
		assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	})
}

func TestInspectManagedDirectory_PermissionAndCancellationFailBeforeMutation(t *testing.T) {
	root := t.TempDir()
	layout := inspectTestLayout(t, filepath.Join(root, "app"), root)
	if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
		t.Fatalf("MkdirAll(app-root) error = %v", err)
	}

	t.Run("permission", func(t *testing.T) {
		api := newProductionPathAPI()
		api.attributes = func(string) (uint32, error) {
			return 0, windows.ERROR_ACCESS_DENIED
		}
		api.makeDirectory = func(string) error {
			t.Fatal("permission failure attempted directory creation")
			return nil
		}
		inspection, err := inspectManagedDirectoryWith(t.Context(), layout, layout.RepoDir(), api)
		if inspection != (DirectoryInspection{}) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("InspectManagedDirectory(permission) = %#v, %v", inspection, err)
		}
	})

	t.Run("pre-cancelled", func(t *testing.T) {
		api := newProductionPathAPI()
		calls := 0
		api.attributes = func(string) (uint32, error) {
			calls++
			return 0, errors.New("unexpected attributes")
		}
		api.makeDirectory = func(string) error {
			calls++
			return errors.New("unexpected mkdir")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		inspection, err := inspectManagedDirectoryWith(ctx, layout, layout.RepoDir(), api)
		if inspection != (DirectoryInspection{}) || !errors.Is(err, context.Canceled) {
			t.Fatalf("InspectManagedDirectory(cancelled) = %#v, %v", inspection, err)
		}
		if calls != 0 {
			t.Fatalf("I/O calls = %d, want 0", calls)
		}
	})
}

func inspectTestLayout(t *testing.T, appRoot, userRoot string) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(appRoot, userRoot)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}
