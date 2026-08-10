//go:build integration

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestIntegration_OfficialUVArtifactFreshRootPublishes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("official Windows uv artifact integration requires Windows")
	}
	archivePath := os.Getenv("AUTO_MAS_UV_INTEGRATION_ZIP")
	if archivePath == "" {
		t.Skip("AUTO_MAS_UV_INTEGRATION_ZIP is not set")
	}
	if got := integrationFileSHA256(t, archivePath); !strings.EqualFold(got, uv.WindowsX64SHA256) {
		t.Fatalf("official archive SHA-256 = %q, want %q", got, uv.WindowsX64SHA256)
	}

	base := t.TempDir()
	appRoot := filepath.Join(base, "app")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(app root) error = %v", err)
	}
	layout := t59Layout(t, appRoot, base)
	protected := integrationCreateProtectedSentinels(t, layout)
	cachePath, err := layout.DownloadFile(uv.WindowsX64Artifact)
	if err != nil {
		t.Fatalf("DownloadFile() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatalf("MkdirAll(download cache) error = %v", err)
	}
	integrationCopyFile(t, archivePath, cachePath)

	for iteration := 1; iteration <= 2; iteration++ {
		stdout, stderr, exitCode := t59Execute(
			t,
			base,
			strings.NewReader(""),
			[]string{"--app-root", appRoot, "--output", "ndjson", "environment", "ensure"},
		)
		if exitCode != protocol.ExitCodeSuccess || stderr != "" {
			t.Fatalf("ensure iteration %d = exit %d, stderr=%q, want success; stdout=%s", iteration, exitCode, stderr, stdout)
		}
		events := parseNDJSON(t, stdout)
		if eventString(events[0], "type") != string(protocol.TypeHello) ||
			eventString(events[len(events)-1], "code") != string(protocol.CodeOK) {
			t.Fatalf("ensure iteration %d terminal events = first %q last code %q", iteration, eventType(events[0]), eventString(events[len(events)-1], "code"))
		}
		operationID := eventString(events[0], "operationId")
		staging, err := layout.UVStagingDir(uv.FixedVersion, operationID)
		if err != nil {
			t.Fatalf("UVStagingDir() error = %v", err)
		}
		t59AssertPathAbsent(t, staging)
		t59AssertPathAbsent(t, layout.MutationStateFile())
	}

	executable, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if info, err := os.Lstat(executable); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published uv.exe = %#v, %v, want regular file", info, err)
	}
	runner, err := uv.NewRunner(uv.RunnerConfig{
		Executable:       executable,
		ProjectDir:       appRoot,
		PythonInstallDir: layout.PythonDir(),
		ProjectEnvDir:    layout.VenvDir(),
		CacheDir:         layout.UVCacheDir(),
	})
	if err != nil {
		t.Fatalf("uv.NewRunner() error = %v", err)
	}
	if err := runner.CheckVersion(t.Context(), uv.FixedVersion, protocol.StageUVVerify, nil); err != nil {
		t.Fatalf("published uv version check error = %v", err)
	}
	stagingPaths, err := filepath.Glob(filepath.Join(layout.UVToolsDir(), uv.FixedVersion+".staging-*"))
	if err != nil {
		t.Fatalf("Glob(uv staging) error = %v", err)
	}
	if len(stagingPaths) != 0 {
		t.Fatalf("uv staging paths = %v, want none", stagingPaths)
	}
	parts, err := filepath.Glob(filepath.Join(layout.DownloadCacheDir(), "*.part*"))
	if err != nil {
		t.Fatalf("Glob(download parts) error = %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("download part files = %v, want none", parts)
	}
	integrationAssertProtectedSentinels(t, protected)
}

func integrationCopyFile(t *testing.T, source, destination string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", source, err)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if closeErr := input.Close(); closeErr != nil {
			t.Errorf("Close(%q) after destination open failure = %v", source, closeErr)
		}
		t.Fatalf("OpenFile(%q) error = %v", destination, err)
	}
	_, copyErr := io.Copy(output, input)
	closeOutputErr := output.Close()
	closeInputErr := input.Close()
	if err := errors.Join(copyErr, closeOutputErr, closeInputErr); err != nil {
		t.Fatalf("copy official archive error = %v", err)
	}
}

func integrationFileSHA256(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatalf("hash official archive error = %v", err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func integrationCreateProtectedSentinels(t *testing.T, layout *config.Layout) map[string]string {
	t.Helper()
	result := make(map[string]string, 6)
	for index, directory := range []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", directory, err)
		}
		path := filepath.Join(directory, "t59-sentinel.txt")
		content := string(rune('a' + index))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
		result[path] = content
	}
	return result
}

func integrationAssertProtectedSentinels(t *testing.T, sentinels map[string]string) {
	t.Helper()
	for path, want := range sentinels {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("protected sentinel %q = %q, %v, want %q", path, got, err, want)
		}
	}
}
