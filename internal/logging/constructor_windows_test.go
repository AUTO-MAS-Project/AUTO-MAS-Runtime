package logging

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func mustCreateTestJunction(t *testing.T, path string, target string) {
	t.Helper()
	script := `$ErrorActionPreference = 'Stop'
$target = $env:AUTO_MAS_LOGGING_JUNCTION_TARGET
$path = $env:AUTO_MAS_LOGGING_JUNCTION_PATH
if ([string]::IsNullOrWhiteSpace($target) -or
    [string]::IsNullOrWhiteSpace($path)) {
    throw 'logging junction environment is incomplete'
}
New-Item -ItemType Junction -Path $path -Target $target -ErrorAction Stop |
    Out-Null`
	command := exec.CommandContext(
		t.Context(),
		"pwsh",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_LOGGING_JUNCTION_TARGET="+target,
		"AUTO_MAS_LOGGING_JUNCTION_PATH="+path,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create required Junction: %v; output=%q", err, output)
	}
	t.Cleanup(func() {
		// 只移除 Junction 本身，禁止递归触碰 target。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove Junction %q: %v", path, err)
		}
	})
}

func createTestFileSymlinkOrSkip(t *testing.T, path string, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("file symlink capability unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove file symlink %q: %v", path, err)
		}
	})
}

func writeExternalSentinel(t *testing.T, external string) (string, []byte) {
	t.Helper()
	want := []byte{0x00, 0xff, 'A', '\r', '\n', 0x7f}
	path := filepath.Join(external, "sentinel.bin")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("WriteFile(external sentinel) error = %v", err)
	}
	return path, want
}

func assertExternalSentinelUnchanged(
	t *testing.T,
	external string,
	sentinel string,
	want []byte,
) {
	t.Helper()
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir(external) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(sentinel) {
		t.Fatalf("external entries = %#v, want only %q", entries, filepath.Base(sentinel))
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("ReadFile(external sentinel) error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("external sentinel bytes = %v, want %v", got, want)
	}
}

func assertNewRejectsUnsafeReparse(
	t *testing.T,
	layout *config.Layout,
	now time.Time,
	external string,
	sentinel string,
	want []byte,
) {
	t.Helper()
	logger, err := New(
		t.Context(),
		layout,
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
		WithClock(func() time.Time { return now }),
	)
	if logger != nil {
		logger.mu.Lock()
		closeErr := closeOwned(logger.writer, logger.files)
		logger.mu.Unlock()
		t.Fatalf(
			"New() returned Logger through a reparse point; closeOwned error = %v",
			closeErr,
		)
	}
	var filesystemErr *filesystem.Error
	if !errors.As(err, &filesystemErr) {
		t.Fatalf("New() error = %v, want *filesystem.Error", err)
	}
	if got := filesystemErr.Code(); got != protocol.CodeUnsafeReparsePoint {
		t.Fatalf("New() error code = %q, want %q", got, protocol.CodeUnsafeReparsePoint)
	}
	assertExternalSentinelUnchanged(t, external, sentinel, want)
}

func TestNew_WindowsRejectsJunctionAtEveryLogAncestorAndCurrentLeaf(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{name: "app root ancestor", level: "app-root"},
		{name: "logs ancestor", level: "logs"},
		{name: "runtime ancestor", level: "runtime"},
		{name: "current log leaf", level: "leaf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			appRoot := filepath.Join(base, "app")
			layout, err := config.NewLayout(appRoot, base)
			if err != nil {
				t.Fatalf("config.NewLayout() error = %v", err)
			}
			external := t.TempDir()
			sentinel, want := writeExternalSentinel(t, external)
			now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.Local)

			var junction string
			switch test.level {
			case "app-root":
				junction = layout.AppRoot()
			case "logs":
				if err := os.MkdirAll(layout.AppRoot(), 0o755); err != nil {
					t.Fatalf("MkdirAll(app root) error = %v", err)
				}
				junction = layout.LogsDir()
			case "runtime":
				if err := os.MkdirAll(layout.LogsDir(), 0o755); err != nil {
					t.Fatalf("MkdirAll(logs) error = %v", err)
				}
				junction = layout.RuntimeLogDir()
			case "leaf":
				if err := os.MkdirAll(layout.RuntimeLogDir(), 0o755); err != nil {
					t.Fatalf("MkdirAll(runtime logs) error = %v", err)
				}
				junction, err = layout.RuntimeLogFile("doctor", now)
				if err != nil {
					t.Fatalf("RuntimeLogFile() error = %v", err)
				}
			default:
				t.Fatalf("unknown test level %q", test.level)
			}

			mustCreateTestJunction(t, junction, external)
			assertNewRejectsUnsafeReparse(
				t,
				layout,
				now,
				external,
				sentinel,
				want,
			)
		})
	}
}

func TestNew_WindowsRejectsCurrentLogFileSymlinkWhenAvailable(t *testing.T) {
	t.Run("file symlink", func(t *testing.T) {
		base := t.TempDir()
		appRoot := filepath.Join(base, "app")
		layout, err := config.NewLayout(appRoot, base)
		if err != nil {
			t.Fatalf("config.NewLayout() error = %v", err)
		}
		if err := os.MkdirAll(layout.RuntimeLogDir(), 0o755); err != nil {
			t.Fatalf("MkdirAll(runtime logs) error = %v", err)
		}
		external := t.TempDir()
		sentinel, want := writeExternalSentinel(t, external)
		now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.Local)
		logPath, err := layout.RuntimeLogFile("doctor", now)
		if err != nil {
			t.Fatalf("RuntimeLogFile() error = %v", err)
		}
		createTestFileSymlinkOrSkip(t, logPath, sentinel)
		assertNewRejectsUnsafeReparse(
			t,
			layout,
			now,
			external,
			sentinel,
			want,
		)
	})
}
