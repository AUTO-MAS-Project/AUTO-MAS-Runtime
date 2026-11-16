package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseAssets_CompleteMatrix(t *testing.T) {
	source := releaseWorkflowSource(t)
	for _, runner := range []string{"ubuntu-24.04", "ubuntu-24.04-arm", "macos-15-intel", "macos-15"} {
		if !strings.Contains(source, "runner: "+runner+"\n") {
			t.Errorf("missing native runner %s", runner)
		}
	}
	for _, gate := range []string{"needs: [package, package-posix]", "scripts/assemble-runtime-release.ps1", "merge-multiple: true", "go run ./scripts/release-smoke", "      - smoke-posix"} {
		if !strings.Contains(source, gate) {
			t.Errorf("missing release gate %s", gate)
		}
	}
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts", "assemble-runtime-release.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"$actual.Count -ne 5", "Compare-Object", "'linux','darwin'", "'amd64','arm64'", "SHA256SUMS.txt", "Get-FileHash"} {
		if !strings.Contains(string(script), gate) {
			t.Errorf("missing asset validation %s", gate)
		}
	}
}
