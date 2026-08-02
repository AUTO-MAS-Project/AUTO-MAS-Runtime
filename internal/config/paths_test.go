package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestLayout_FixedPathsMatchSpecification(t *testing.T) {
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "RepoDir", got: layout.RepoDir(), want: filepath.Join(root, "repo")},
		{name: "RepoVersionFile", got: layout.RepoVersionFile(), want: filepath.Join(root, "repo", "res", "version.json")},
		{name: "StateDir", got: layout.StateDir(), want: filepath.Join(root, "runtime-state")},
		{name: "BackendStateFile", got: layout.BackendStateFile(), want: filepath.Join(root, "runtime-state", "backend.json")},
		{name: "MutationStateFile", got: layout.MutationStateFile(), want: filepath.Join(root, "runtime-state", "mutation.json")},
		{name: "UpdateStateFile", got: layout.UpdateStateFile(), want: filepath.Join(root, "runtime-state", "update.json")},
		{name: "EnvironmentStateFile", got: layout.EnvironmentStateFile(), want: filepath.Join(root, "runtime-state", "environment.json")},
		{name: "RuntimeDir", got: layout.RuntimeDir(), want: filepath.Join(root, "runtime")},
		{name: "UVToolsDir", got: layout.UVToolsDir(), want: filepath.Join(root, "runtime", "tools", "uv")},
		{name: "PythonDir", got: layout.PythonDir(), want: filepath.Join(root, "runtime", "environment", "python")},
		{name: "VenvDir", got: layout.VenvDir(), want: filepath.Join(root, "runtime", "environment", "venv")},
		{name: "RuntimeCacheDir", got: layout.RuntimeCacheDir(), want: filepath.Join(root, "runtime", "cache")},
		{name: "UVCacheDir", got: layout.UVCacheDir(), want: filepath.Join(root, "runtime", "cache", "uv")},
		{name: "DownloadCacheDir", got: layout.DownloadCacheDir(), want: filepath.Join(root, "runtime", "cache", "downloads")},
		{name: "BuildCacheDir", got: layout.BuildCacheDir(), want: filepath.Join(root, "runtime", "cache", "build")},
		{name: "LogsDir", got: layout.LogsDir(), want: filepath.Join(root, "logs")},
		{name: "RuntimeLogDir", got: layout.RuntimeLogDir(), want: filepath.Join(root, "logs", "runtime")},
		{name: "ConfigDir", got: layout.ConfigDir(), want: filepath.Join(root, "config")},
		{name: "DataDir", got: layout.DataDir(), want: filepath.Join(root, "data")},
		{name: "HistoryDir", got: layout.HistoryDir(), want: filepath.Join(root, "history")},
		{name: "ScriptDir", got: layout.ScriptDir(), want: filepath.Join(root, "script")},
		{name: "DebugDir", got: layout.DebugDir(), want: filepath.Join(root, "debug")},
		{name: "PluginsDir", got: layout.PluginsDir(), want: filepath.Join(root, "plugins")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestLayout_FixedPathsAreAbsolute(t *testing.T) {
	layout := newFixedPathsLayout(t)
	for _, test := range fixedPaths(layout) {
		t.Run(test.name, func(t *testing.T) {
			if !filepath.IsAbs(test.path) {
				t.Fatalf("got %q, want absolute path", test.path)
			}
		})
	}
}

func TestLayout_StateFilesAreDistinct(t *testing.T) {
	layout := newFixedPathsLayout(t)
	stateFiles := []struct {
		name string
		path string
	}{
		{name: "BackendStateFile", path: layout.BackendStateFile()},
		{name: "MutationStateFile", path: layout.MutationStateFile()},
		{name: "UpdateStateFile", path: layout.UpdateStateFile()},
		{name: "EnvironmentStateFile", path: layout.EnvironmentStateFile()},
	}

	seen := make(map[string]struct{}, len(stateFiles))
	for _, stateFile := range stateFiles {
		t.Run(stateFile.name, func(t *testing.T) {
			if _, ok := seen[stateFile.path]; ok {
				t.Fatalf("got duplicate state file path %q", stateFile.path)
			}
			seen[stateFile.path] = struct{}{}
			if got, want := filepath.Dir(stateFile.path), layout.StateDir(); got != want {
				t.Fatalf("got parent %q, want %q", got, want)
			}
		})
	}
}

func TestLayout_AllFixedPathsStayWithinAppRoot(t *testing.T) {
	layout := newFixedPathsLayout(t)
	for _, test := range fixedPaths(layout) {
		t.Run(test.name, func(t *testing.T) {
			rel, err := filepath.Rel(layout.AppRoot(), test.path)
			if err != nil {
				t.Fatalf("Rel(%q, %q) error = %v", layout.AppRoot(), test.path, err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("got path outside app root: %q", test.path)
			}
		})
	}
}

func TestLayout_ConstructionAndGettersDoNotTouchFilesystem(t *testing.T) {
	base := t.TempDir()
	appRoot := filepath.Join(base, "does-not-exist")
	if _, err := os.Stat(appRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want errors.Is(_, fs.ErrNotExist)", appRoot, err)
	}

	layout, err := config.NewLayout(appRoot, base)
	if err != nil {
		t.Fatalf("NewLayout(%q, %q) error = %v", appRoot, base, err)
	}
	_ = layout.AppRoot()
	_ = layout.IdentityKey()
	_ = layout.ProtectedRootDirs()
	for _, fixed := range fixedPaths(layout) {
		_ = fixed.path
	}

	dynamicCalls := []struct {
		name string
		call func() error
	}{
		{name: "RepoUpdateDir", call: func() error { _, err := layout.RepoUpdateDir("operation"); return err }},
		{name: "RepoPreviousDir", call: func() error { _, err := layout.RepoPreviousDir("operation"); return err }},
		{name: "UVVersionDir", call: func() error { _, err := layout.UVVersionDir("0.8.0"); return err }},
		{name: "UVExecutable", call: func() error { _, err := layout.UVExecutable("0.8.0"); return err }},
		{name: "DownloadFile", call: func() error { _, err := layout.DownloadFile("uv.zip"); return err }},
		{name: "DownloadPartFile", call: func() error { _, err := layout.DownloadPartFile("uv.zip"); return err }},
		{name: "UVStagingDir", call: func() error { _, err := layout.UVStagingDir("0.8.0", "operation"); return err }},
		{name: "RuntimeLogFile", call: func() error {
			_, err := layout.RuntimeLogFile("diagnose", time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC))
			return err
		}},
	}
	for _, dynamic := range dynamicCalls {
		t.Run(dynamic.name, func(t *testing.T) {
			if err := dynamic.call(); err != nil {
				t.Fatalf("%s() error = %v", dynamic.name, err)
			}
		})
	}

	if _, err := os.Stat(appRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) after getters error = %v, want errors.Is(_, fs.ErrNotExist)", appRoot, err)
	}
}

type fixedPath struct {
	name string
	path string
}

func fixedPaths(layout *config.Layout) []fixedPath {
	return []fixedPath{
		{name: "RepoDir", path: layout.RepoDir()},
		{name: "RepoVersionFile", path: layout.RepoVersionFile()},
		{name: "StateDir", path: layout.StateDir()},
		{name: "BackendStateFile", path: layout.BackendStateFile()},
		{name: "MutationStateFile", path: layout.MutationStateFile()},
		{name: "UpdateStateFile", path: layout.UpdateStateFile()},
		{name: "EnvironmentStateFile", path: layout.EnvironmentStateFile()},
		{name: "RuntimeDir", path: layout.RuntimeDir()},
		{name: "UVToolsDir", path: layout.UVToolsDir()},
		{name: "PythonDir", path: layout.PythonDir()},
		{name: "VenvDir", path: layout.VenvDir()},
		{name: "RuntimeCacheDir", path: layout.RuntimeCacheDir()},
		{name: "UVCacheDir", path: layout.UVCacheDir()},
		{name: "DownloadCacheDir", path: layout.DownloadCacheDir()},
		{name: "BuildCacheDir", path: layout.BuildCacheDir()},
		{name: "LogsDir", path: layout.LogsDir()},
		{name: "RuntimeLogDir", path: layout.RuntimeLogDir()},
		{name: "ConfigDir", path: layout.ConfigDir()},
		{name: "DataDir", path: layout.DataDir()},
		{name: "HistoryDir", path: layout.HistoryDir()},
		{name: "ScriptDir", path: layout.ScriptDir()},
		{name: "DebugDir", path: layout.DebugDir()},
		{name: "PluginsDir", path: layout.PluginsDir()},
	}
}

func newFixedPathsLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	return layout
}
