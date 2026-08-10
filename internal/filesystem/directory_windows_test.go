package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestPrepareManagedDirectory_CreatesAndPinsExactTarget(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-operation")
	lease, err := PrepareManagedDirectory(t.Context(), layout, target)
	if err != nil {
		t.Fatalf("PrepareManagedDirectory() error = %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("cleanup lease.Close() error = %v", err)
		}
	})
	wantPath := mustCanonicalize(t, target).String()
	if lease.Path() != wantPath {
		t.Fatalf("lease.Path() = %q, want %q", lease.Path(), wantPath)
	}
	info, err := os.Lstat(lease.Path())
	if err != nil {
		t.Fatalf("Lstat(target) error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("target mode = %v, want directory", info.Mode())
	}
	if err := os.Remove(lease.Path()); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("Remove(target) error = %v, want sharing violation while lease is open", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close() error = %v", err)
	}
	if err := os.Remove(lease.Path()); err != nil {
		t.Fatalf("Remove(target) after close error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second lease.Close() error = %v, want idempotent close", err)
	}
}

func TestPrepareManagedDirectory_RejectsExistingAndCancelledTargets(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-existing")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}
	if lease, err := PrepareManagedDirectory(t.Context(), layout, target); lease != nil || err == nil {
		t.Fatalf("PrepareManagedDirectory(existing) = lease:%v err:%v, want rejection", lease, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelledTarget := filepath.Join(layout.AppRoot(), "repo.update-cancelled")
	if lease, err := PrepareManagedDirectory(ctx, layout, cancelledTarget); lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareManagedDirectory(cancelled) = lease:%v err:%v, want cancellation", lease, err)
	}
	if _, err := os.Lstat(cancelledTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled target exists: %v", err)
	}
}

func TestPrepareManagedDirectory_CancellationAfterParentPinStopsBeforeCreate(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-cancelled-after-pin")
	ctx, cancel := context.WithCancel(t.Context())
	api := newProductionPathAPI()
	caseSensitive := api.caseSensitive
	api.caseSensitive = func(handle windows.Handle) (bool, error) {
		value, err := caseSensitive(handle)
		if err == nil {
			cancel()
		}
		return value, err
	}

	lease, err := prepareManagedDirectoryWithAPI(ctx, layout, target, api)
	if lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf(
			"prepareManagedDirectoryWithAPI() = lease:%v err:%v, want pre-create cancellation",
			lease,
			err,
		)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled target exists: %v", statErr)
	}
}

func TestPrepareManagedDirectory_CancellationAfterCreateRemovesCreatedTarget(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-cancelled-after-create")
	ctx, cancel := context.WithCancel(t.Context())
	api := newProductionPathAPI()
	ntCreateRelative := api.ntCreateRelative
	identity := api.identity
	createdHandle := windows.InvalidHandle
	api.ntCreateRelative = func(
		parent windows.Handle,
		name string,
		spec ntCreateSpec,
	) (windows.Handle, error) {
		handle, err := ntCreateRelative(parent, name, spec)
		if err == nil {
			createdHandle = handle
		}
		return handle, err
	}
	api.identity = func(handle windows.Handle) (objectIdentity, error) {
		value, err := identity(handle)
		if err == nil && handle == createdHandle {
			cancel()
		}
		return value, err
	}

	lease, err := prepareManagedDirectoryWithAPI(ctx, layout, target, api)
	if lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf(
			"prepareManagedDirectoryWithAPI() = lease:%v err:%v, want post-create cancellation",
			lease,
			err,
		)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled target exists: %v", statErr)
	}
}

func TestPrepareManagedDirectory_CloseFailurePreservesCreatedTarget(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("os.Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-close-failure")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	api := newProductionPathAPI()
	originalCreate := api.ntCreateRelative
	originalIdentity := api.identity
	originalOpenRelative := api.openRelative
	originalClose := api.closeHandle
	createdHandle := windows.InvalidHandle
	closeCalls := 0
	deleteOpenCalls := 0
	api.ntCreateRelative = func(
		parent windows.Handle,
		name string,
		spec ntCreateSpec,
	) (windows.Handle, error) {
		handle, err := originalCreate(parent, name, spec)
		if err == nil {
			createdHandle = handle
		}
		return handle, err
	}
	api.identity = func(handle windows.Handle) (objectIdentity, error) {
		value, err := originalIdentity(handle)
		if err == nil && handle == createdHandle {
			cancel()
		}
		return value, err
	}
	api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		if spec.access&windows.DELETE != 0 {
			deleteOpenCalls++
		}
		return originalOpenRelative(parent, name, spec)
	}
	api.closeHandle = func(handle windows.Handle) error {
		if handle == createdHandle {
			closeCalls++
			return errors.New("injected close failure")
		}
		return originalClose(handle)
	}

	lease, err := prepareManagedDirectoryWithAPI(ctx, layout, target, api)
	if lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("prepareManagedDirectoryWithAPI() = lease:%v err:%v, want cancellation", lease, err)
	}
	if got, want := closeCalls, managedDirectoryCloseAttempts; got != want {
		t.Fatalf("created handle close calls = %d, want %d", got, want)
	}
	if deleteOpenCalls != 0 {
		t.Fatalf("delete reopen calls = %d, want 0 after close exhaustion", deleteOpenCalls)
	}
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("created target was removed after close exhaustion: %v", statErr)
	}
	if createdHandle != windows.InvalidHandle {
		if closeErr := originalClose(createdHandle); closeErr != nil {
			t.Fatalf("close injected handle for test cleanup: %v", closeErr)
		}
	}
	if removeErr := os.Remove(target); removeErr != nil {
		t.Fatalf("remove preserved target after test cleanup: %v", removeErr)
	}
}

func TestPrepareManagedDirectory_RejectsReparseReplacement(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatalf("MkdirAll(external) error = %v", err)
	}
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app-root) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	target := filepath.Join(layout.AppRoot(), "repo.update-race")
	inspection, err := InspectManagedDirectory(t.Context(), layout, target)
	if err != nil || inspection.Exists {
		t.Fatalf("InspectManagedDirectory() = %#v, %v, want absent", inspection, err)
	}
	if err := os.Symlink(external, target); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if lease, err := PrepareManagedDirectory(t.Context(), layout, target); lease != nil || err == nil {
		t.Fatalf("PrepareManagedDirectory(reparse replacement) = lease:%v err:%v, want rejection", lease, err)
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir(external) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("external directory was modified: %v", entries)
	}
}

func TestPinManagedDirectory_HoldsExistingIdentityUntilClose(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.MkdirAll(filepath.Join(appRoot, "repo"), 0o700); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	layout := directoryTestLayout(t, appRoot, root)
	inspection, err := InspectManagedDirectory(t.Context(), layout, layout.RepoDir())
	if err != nil || !inspection.Exists || inspection.Identity == nil {
		t.Fatalf("InspectManagedDirectory() = %#v, %v, want existing identity", inspection, err)
	}
	lease, err := PinManagedDirectory(t.Context(), layout, layout.RepoDir())
	if err != nil {
		t.Fatalf("PinManagedDirectory() error = %v", err)
	}
	if lease.Identity() == nil || !matchesDirectoryIdentity(inspection.Identity, *lease.Identity()) {
		t.Fatalf("lease identity = %#v, want inspected identity %#v", lease.Identity(), inspection.Identity)
	}
	retired := filepath.Join(appRoot, "repo.retired")
	if err := os.Rename(layout.RepoDir(), retired); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("Rename(repo while pinned) error = %v, want sharing violation", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close() error = %v", err)
	}
	if err := os.Rename(layout.RepoDir(), retired); err != nil {
		t.Fatalf("Rename(repo after close) error = %v", err)
	}
}

func TestDirectoryLease_RetriesAfterCloseFailure(t *testing.T) {
	closeCalls := 0
	lease := newDirectoryLease("C:\\managed\\repo.update-test", func() error {
		closeCalls++
		if closeCalls == 1 {
			return errors.New("transient close failure")
		}
		return nil
	})
	if err := lease.Close(); err == nil {
		t.Fatal("first lease.Close() error = nil, want close failure")
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second lease.Close() error = %v, want nil", err)
	}
	if got, want := closeCalls, 2; got != want {
		t.Fatalf("close calls = %d, want %d", got, want)
	}
}

func directoryTestLayout(t *testing.T, appRoot, base string) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(appRoot, base)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}
