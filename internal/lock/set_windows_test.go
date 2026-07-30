//go:build windows

package lock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type countingStartupAPI struct {
	windowsAPI

	mu               sync.Mutex
	duplicateCalls   int
	createMutexCalls int
}

func (a *countingStartupAPI) duplicateHandle(
	sourceProcess windows.Handle,
	source windows.Handle,
	targetProcess windows.Handle,
	target *windows.Handle,
	desiredAccess uint32,
	inheritHandle bool,
	options uint32,
) error {
	a.mu.Lock()
	a.duplicateCalls++
	a.mu.Unlock()
	return a.windowsAPI.duplicateHandle(
		sourceProcess,
		source,
		targetProcess,
		target,
		desiredAccess,
		inheritHandle,
		options,
	)
}

func (a *countingStartupAPI) createMutex(
	security *windows.SecurityAttributes,
	initialOwner bool,
	name *uint16,
) (windows.Handle, error) {
	a.mu.Lock()
	a.createMutexCalls++
	a.mu.Unlock()
	return a.windowsAPI.createMutex(security, initialOwner, name)
}

func (a *countingStartupAPI) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.duplicateCalls, a.createMutexCalls
}

func TestNewSet_RejectsNilInputsAndMissingRoot(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if _, err := NewSet(nil, layout); err == nil {
		t.Fatal("NewSet(nil context) error = nil, want rejection")
	}
	if _, err := NewSet(t.Context(), nil); err == nil {
		t.Fatal("NewSet(nil layout) error = nil, want rejection")
	}

	missing := filepath.Join(t.TempDir(), "missing")
	missingLayout, err := config.NewLayout(
		missing,
		filepath.Dir(missing),
	)
	if err != nil {
		t.Fatalf("config.NewLayout(missing) error = %v", err)
	}
	if _, err := NewSet(t.Context(), missingLayout); err == nil {
		t.Fatal("NewSet(missing root) error = nil, want rejection")
	}
}

func TestNewSet_RejectsRealFileBeforeWorker(t *testing.T) {
	parent := t.TempDir()
	appRoot := filepath.Join(parent, "app-root.txt")
	if err := os.WriteFile(appRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, parent)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	api := &countingStartupAPI{windowsAPI: systemWindowsAPI{}}
	set, err := newSet(t.Context(), layout, api)
	if err == nil {
		if closeErr := set.Close(); closeErr != nil {
			t.Errorf("Set.Close() error = %v", closeErr)
		}
		t.Fatal("newSet() error = nil, want regular-file rejection")
	}
	duplicateCalls, createMutexCalls := api.counts()
	if duplicateCalls != 0 || createMutexCalls != 0 {
		t.Fatalf(
			"DuplicateHandle/CreateMutex calls = %d/%d, want 0/0",
			duplicateCalls,
			createMutexCalls,
		)
	}
}

func TestNewSet_OpensExistingNamedMutex(t *testing.T) {
	appRoot := t.TempDir()
	layout, err := config.NewLayout(appRoot, filepath.Dir(appRoot))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	first, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("first NewSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("cleanup first Set.Close() error = %v", err)
		}
	})
	second, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("second NewSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("cleanup second Set.Close() error = %v", err)
		}
	})
	if err := second.Close(); err != nil {
		t.Fatalf("second Set.Close() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Set.Close() error = %v", err)
	}
}
