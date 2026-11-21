package release

import (
	"os"
	"path/filepath"
	"regexp"
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
	for _, required := range []string{"permissions:\n  contents: read", "runner: macos-15", "runner: macos-26", "MACOSX_DEPLOYMENT_TARGET: \"15.0\"", "shell: pwsh", "AUTO_MAS_TELEMETRY: disabled", "persist-credentials: false", "MACOSX_DEPLOYMENT_TARGET", "uname -m", "sw_vers -productVersion", "go build -buildvcs=false", "go test -race", "-count=100", "TestExecute_PlatformBoundary", "TestLayout_POSIXIdentity", "TestPOSIXState_", "TestSystemPIDProbe_POSIX", "-fs 'Case-sensitive APFS'", "hdiutil detach", "if ($LASTEXITCODE -ne 0)", "if: always()", "if: ${{ !cancelled() && steps.platform.outcome == 'success' }}"} {
		if !strings.Contains(source, required) {
			t.Errorf("missing workflow boundary %q", required)
		}
	}
	for _, forbidden := range []string{"continue-on-error", "secrets.", "contents: write", "pull_request_target", "macos-latest", "macos-14", "git push", "gh release", "-fs APFSX"} {
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

// TestMacOSWorkflow_MatchesWindowsToolchain 锁定 macOS 原型与 Windows CI 使用同一 Go 工具链。
// 两个 workflow 各自硬编码版本，缺少断言时容易单侧漂移。
func TestMacOSWorkflow_MatchesWindowsToolchain(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("missing test source path")
	}
	workflows := filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows")
	macOS, err := os.ReadFile(filepath.Join(workflows, "macos-prototype.yml"))
	if err != nil {
		t.Fatal(err)
	}
	windows, err := os.ReadFile(filepath.Join(workflows, "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	setupPattern := regexp.MustCompile(`actions/setup-go@([0-9a-f]{40})`)
	versionPattern := regexp.MustCompile(`go-version:\s*["']([0-9]+\.[0-9]+\.[0-9]+)["']`)

	macOSSetup := setupPattern.FindStringSubmatch(string(macOS))
	windowsSetup := setupPattern.FindStringSubmatch(string(windows))
	if macOSSetup == nil || windowsSetup == nil {
		t.Fatal("missing pinned actions/setup-go reference")
	}
	if macOSSetup[1] != windowsSetup[1] {
		t.Fatalf("setup-go pin = %q, want %q to match Windows CI", macOSSetup[1], windowsSetup[1])
	}

	macOSVersion := versionPattern.FindStringSubmatch(string(macOS))
	windowsVersion := versionPattern.FindStringSubmatch(string(windows))
	if macOSVersion == nil || windowsVersion == nil {
		t.Fatal("missing go-version pin")
	}
	if macOSVersion[1] != windowsVersion[1] {
		t.Fatalf("go-version = %q, want %q to match Windows CI", macOSVersion[1], windowsVersion[1])
	}
}
