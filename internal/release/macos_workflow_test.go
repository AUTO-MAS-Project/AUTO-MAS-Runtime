package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMacOSWorkflow_PrototypeBoundary(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("missing test source path")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "macos-prototype.yml"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, required := range []string{"permissions:\n  contents: read", "macos-14", "macos-15", "shell: pwsh", "AUTO_MAS_TELEMETRY: disabled", "persist-credentials: false", "MACOSX_DEPLOYMENT_TARGET", "uname -m", "sw_vers -productVersion", "go build -buildvcs=false", "go test -race", "-count=100", "TestExecute_PlatformBoundary", "TestLayout_POSIXIdentity", "-fs 'Case-sensitive APFS'", "hdiutil detach", "if ($LASTEXITCODE -ne 0)", "if: always()", "if: ${{ !cancelled() && steps.platform.outcome == 'success' }}"} {
		if !strings.Contains(source, required) {
			t.Errorf("missing workflow boundary %q", required)
		}
	}
	for _, forbidden := range []string{"continue-on-error", "secrets.", "contents: write", "pull_request_target", "macos-latest", "git push", "gh release", "-fs APFSX"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("unexpected prototype workflow action %q", forbidden)
		}
	}
	directoryStep := strings.Index(source, "name: Run filesystem mechanism probes")
	sensitiveStep := strings.Index(source, "name: Repeat directory probes on case-sensitive APFS")
	launchdStep := strings.Index(source, "name: Run launchd mechanism probes")
	if directoryStep < 0 || sensitiveStep <= directoryStep || launchdStep <= sensitiveStep {
		t.Fatal("filesystem evidence must run before launchd probes")
	}
}
