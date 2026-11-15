//go:build integration

package uv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// TestIntegration_OfficialUVArtifact 验证显式提供的官方 uv ZIP 可被真实解压和执行。
func TestIntegration_OfficialUVArtifact(t *testing.T) {
	artifact, err := ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := os.Getenv("AUTO_MAS_UV_INTEGRATION_ARCHIVE")
	if archivePath == "" {
		archivePath = os.Getenv("AUTO_MAS_UV_INTEGRATION_ZIP")
	}
	if archivePath == "" {
		t.Skip("AUTO_MAS_UV_INTEGRATION_ZIP is not set")
	}
	if err := verifySHA256(t.Context(), archivePath, artifact.SHA256); err != nil {
		t.Fatalf("verifySHA256() error = %v", err)
	}
	stagingDir := t.TempDir()
	var extractor ArchiveExtractor = zipExtractor{}
	if runtime.GOOS != "windows" {
		extractor = tarExtractor{}
	}
	if err := extractor.Extract(t.Context(), archivePath, stagingDir); err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != config.UVExecutableName() {
		t.Fatalf("staging entries = %v, want only uv.exe", entries)
	}
	root := t.TempDir()
	runner, err := NewRunner(RunnerConfig{
		Executable:       filepath.Join(stagingDir, config.UVExecutableName()),
		ProjectDir:       root,
		PythonInstallDir: filepath.Join(root, "python"),
		ProjectEnvDir:    filepath.Join(root, "venv"),
		CacheDir:         filepath.Join(root, "cache"),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if err := runner.CheckVersion(t.Context(), FixedVersion, protocol.StageUVVerify, nil); err != nil {
		t.Fatalf("CheckVersion() error = %v", err)
	}
}
