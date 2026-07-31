package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestLoggerWindows_RotatesAndRetainsThroughRuntimeLogFiles(t *testing.T) {
	base := t.TempDir()
	appRoot := filepath.Join(base, "app")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(app root) error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, base)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	location := time.FixedZone("CST", 8*60*60)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, location)
	dayTwo := time.Date(2026, 7, 30, 0, 0, 1, 0, location)
	clock := &sequenceClock{values: []time.Time{dayOne, dayTwo}}
	var stderr bytes.Buffer
	logger, err := New(
		t.Context(),
		layout,
		&stderr,
		"doctor",
		"01JTEST",
		WithClock(clock.now),
		WithRetention(RetentionPolicy{MaxAgeDays: 2, MaxFilesPerCommand: 2}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		if closeErr := closeOwned(logger.writer, logger.files); closeErr != nil {
			t.Errorf("closeOwned() error = %v", closeErr)
		}
	})
	oldPath := logger.LogPath()
	stalePath, err := layout.RuntimeLogFile("workspace-sync", dayOne.AddDate(0, 0, -10))
	if err != nil {
		t.Fatalf("RuntimeLogFile(stale) error = %v", err)
	}
	if err := os.WriteFile(stalePath, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(stale) error = %v", err)
	}

	result, err := logger.Record(t.Context(), LevelInfo, "after midnight", nil)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if !result.Rotated || !result.FileWritten {
		t.Fatalf("Record() result = %#v, want rotated and written", result)
	}
	newPath := logger.LogPath()
	if newPath == oldPath || !filepath.IsAbs(newPath) {
		t.Fatalf("paths = old %q/new %q, want distinct absolute paths", oldPath, newPath)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(stale) error = %v, want not exist", err)
	}
	for _, path := range []string{oldPath, newPath} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, readErr)
		}
		for index, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatalf("%s line %d is invalid JSON: %q", path, index, line)
			}
		}
	}
}
