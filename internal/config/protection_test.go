package config_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestLayout_ProtectedRootDirs(t *testing.T) {
	layout := newFixedPathsLayout(t)
	want := []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
		layout.LogsDir(),
	}

	got := layout.ProtectedRootDirs()
	if len(got) != len(want) {
		t.Fatalf("ProtectedRootDirs() length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ProtectedRootDirs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLayout_ProtectedRootDirsReturnsDefensiveCopy(t *testing.T) {
	layout := newFixedPathsLayout(t)
	want := []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
		layout.LogsDir(),
	}

	got := layout.ProtectedRootDirs()
	got[0] = "changed"
	got = append(got, "added")

	second := layout.ProtectedRootDirs()
	if len(second) != len(want) {
		t.Fatalf("second ProtectedRootDirs() length = %d, want %d", len(second), len(want))
	}
	for i := range want {
		if second[i] != want[i] {
			t.Fatalf("second ProtectedRootDirs()[%d] = %q, want %q", i, second[i], want[i])
		}
	}
}

func TestLayout_GettersAreConcurrentSafe(t *testing.T) {
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}

	const workers = 32
	const iterations = 100
	start := make(chan struct{})
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for range iterations {
				_ = layout.AppRoot()
				_ = layout.IdentityKey()
				_ = layout.ProtectedRootDirs()
				for _, fixed := range fixedPaths(layout) {
					_ = fixed.path
				}
				if _, err := layout.RepoUpdateDir("operation"); err != nil {
					errs <- fmt.Errorf("RepoUpdateDir: %w", err)
					return
				}
				if _, err := layout.RepoPreviousDir("operation"); err != nil {
					errs <- fmt.Errorf("RepoPreviousDir: %w", err)
					return
				}
				if _, err := layout.UVVersionDir("0.8.0"); err != nil {
					errs <- fmt.Errorf("UVVersionDir: %w", err)
					return
				}
				if _, err := layout.UVExecutable("0.8.0"); err != nil {
					errs <- fmt.Errorf("UVExecutable: %w", err)
					return
				}
				if _, err := layout.DownloadFile("uv.zip"); err != nil {
					errs <- fmt.Errorf("DownloadFile: %w", err)
					return
				}
				if _, err := layout.DownloadPartFile("uv.zip"); err != nil {
					errs <- fmt.Errorf("DownloadPartFile: %w", err)
					return
				}
				if _, err := layout.UVStagingDir("0.8.0", "operation"); err != nil {
					errs <- fmt.Errorf("UVStagingDir: %w", err)
					return
				}
				if _, err := layout.RuntimeLogFile("diagnose", time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)); err != nil {
					errs <- fmt.Errorf("RuntimeLogFile: %w", err)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
