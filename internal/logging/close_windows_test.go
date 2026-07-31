package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func assertRenameBlocked(t *testing.T, path string) {
	t.Helper()
	moved := path + ".blocked-check"
	err := os.Rename(path, moved)
	if err != nil {
		return
	}
	if restoreErr := os.Rename(moved, path); restoreErr != nil {
		t.Fatalf("rename %q unexpectedly succeeded and restore failed: %v", path, restoreErr)
	}
	t.Fatalf("Rename(%q) succeeded while Logger handles were open", path)
}

func assertRenameRoundTrip(t *testing.T, path string) {
	t.Helper()
	moved := path + ".released-check"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("Rename(%q after Close) error = %v", path, err)
	}
	if err := os.Rename(moved, path); err != nil {
		t.Fatalf("restore Rename(%q) error = %v", moved, err)
	}
}

func TestLoggerWindows_CloseReleasesRuntimeLogHandles(t *testing.T) {
	base := t.TempDir()
	appRoot := filepath.Join(base, "app")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(app root) error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, base)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	logger, err := New(
		t.Context(),
		layout,
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := logger.Close(); closeErr != nil {
			t.Errorf("cleanup Logger.Close() error = %v", closeErr)
		}
	})

	for _, path := range []string{
		layout.RuntimeLogDir(),
		layout.LogsDir(),
		layout.AppRoot(),
	} {
		assertRenameBlocked(t, path)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for _, path := range []string{
		layout.RuntimeLogDir(),
		layout.LogsDir(),
		layout.AppRoot(),
	} {
		assertRenameRoundTrip(t, path)
	}
}
