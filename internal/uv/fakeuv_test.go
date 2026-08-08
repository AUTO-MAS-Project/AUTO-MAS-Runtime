package uv

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFakeUV_ReplaysConfiguredProcess(t *testing.T) {
	root := t.TempDir()
	executable := buildFakeUV(t, root)
	configPath := writeFakeUVConfig(t, root, map[string]any{
		"exitCode": 7,
		"stdout":   []string{"progress line"},
		"stderr":   []string{"warning line"},
	})
	projectDir := filepath.Join(root, "repo")
	pythonDir := filepath.Join(root, "python")
	venvDir := filepath.Join(root, "venv")
	cacheDir := filepath.Join(root, "cache")
	for _, path := range []string{projectDir, pythonDir, venvDir, cacheDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := NewRunner(RunnerConfig{
		Executable:       executable,
		ProjectDir:       projectDir,
		PythonInstallDir: pythonDir,
		ProjectEnvDir:    venvDir,
		CacheDir:         cacheDir,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	result, err := runner.Run(context.Background(), []string{"sync", "--locked"}, RunOptions{
		Stage:       "dependencies.sync",
		Environment: map[string]string{"FAKE_UV_CONFIG": configPath},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want non-zero fake process failure")
	}
	if !strings.Contains(err.Error(), "uv 执行失败") {
		t.Fatalf("Run() error = %v, want structured uv execution failure", err)
	}
	if result.ExitCode != 7 || !strings.Contains(result.Stdout, "progress line") || !strings.Contains(result.Stderr, "warning line") {
		t.Fatalf("Run() result = %#v, want exit 7 and replayed streams", result)
	}
}

func TestFakeUV_RecordsArgumentsAndEnvironment(t *testing.T) {
	root := t.TempDir()
	executable := buildFakeUV(t, root)
	configPath := writeFakeUVConfig(t, root, map[string]any{"exitCode": 0})
	recordPath := filepath.Join(root, "record.json")
	projectDir := filepath.Join(root, "repo")
	pythonDir := filepath.Join(root, "python")
	venvDir := filepath.Join(root, "venv")
	cacheDir := filepath.Join(root, "cache")
	for _, path := range []string{projectDir, pythonDir, venvDir, cacheDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := NewRunner(RunnerConfig{
		Executable:       executable,
		ProjectDir:       projectDir,
		PythonInstallDir: pythonDir,
		ProjectEnvDir:    venvDir,
		CacheDir:         cacheDir,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Run(context.Background(), []string{"python", "install", "3.12.10"}, RunOptions{
		Stage: "python.install",
		Environment: map[string]string{
			"FAKE_UV_CONFIG": configPath,
			"FAKE_UV_RECORD": recordPath,
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	payload, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("ReadFile(record) error = %v", err)
	}
	var record struct {
		Arguments   []string          `json:"arguments"`
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if strings.Join(record.Arguments, " ") != "python install 3.12.10" {
		t.Fatalf("arguments = %#v, want exact argv", record.Arguments)
	}
	wants := map[string]string{
		uvPythonInstallDirEnv: pythonDir,
		uvCacheDirEnv:         cacheDir,
		uvProjectEnvironment:  venvDir,
		uvManagedPythonEnv:    "1",
		uvNoModifyPathEnv:     "1",
		uvPythonInstallBinEnv: "0",
		uvColorEnv:            "never",
		uvNoProgressEnv:       "1",
	}
	for key, want := range wants {
		if got := record.Environment[key]; got != want {
			t.Errorf("environment[%q] = %q, want %q", key, got, want)
		}
	}
}

func buildFakeUV(t *testing.T, root string) string {
	t.Helper()
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workspace = filepath.Clean(filepath.Join(workspace, "..", ".."))
	name := "fakeuv.exe"
	if runtime.GOOS != "windows" {
		name = "fakeuv"
	}
	output := filepath.Join(root, name)
	fakePackage, err := filepath.Abs(filepath.Join(workspace, "testdata", "fakeuv"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", output, fakePackage)
	command.Dir = workspace
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake uv: %v\n%s", err, output)
	}
	return output
}

func writeFakeUVConfig(t *testing.T, root string, value map[string]any) string {
	t.Helper()
	path := filepath.Join(root, "config.json")
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
