// Package release 包含 GitHub Release workflow 的静态契约测试。
package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseWorkflow_PackageAndPublishContract(t *testing.T) {
	source := releaseWorkflowSource(t)
	required := []struct {
		name    string
		snippet string
	}{
		{name: "tag trigger", snippet: "on:\n  push:\n    tags:\n      - \"v*\""},
		{name: "Windows runner", snippet: "runs-on: windows-latest"},
		{name: "checkout", snippet: "uses: actions/checkout@v6"},
		{name: "setup Go", snippet: "uses: actions/setup-go@v6"},
		{name: "Go version file", snippet: "go-version-file: go.mod"},
		{name: "test command", snippet: "& go test ./..."},
		{name: "test exit check", snippet: "throw \"release tests failed\""},
		{name: "Windows target", snippet: "$env:GOOS = \"windows\""},
		{name: "amd64 target", snippet: "$env:GOARCH = \"amd64\""},
		{name: "trimpath build", snippet: "& go build -trimpath -buildvcs=false -ldflags $ldflags"},
		{name: "version ldflag", snippet: "internal/version.Version=$($env:RELEASE_TAG)"},
		{name: "commit ldflag", snippet: "internal/version.Commit=$($env:SOURCE_COMMIT)"},
		{name: "build date ldflag", snippet: "internal/version.BuildDate=$($env:BUILD_DATE)"},
		{name: "telemetry disabled during release tests", snippet: "AUTO_MAS_TELEMETRY: disabled"},
		{name: "optional Sentry secret", snippet: "AUTO_MAS_SENTRY_DSN: ${{ secrets.AUTO_MAS_SENTRY_DSN }}"},
		{name: "Sentry DSN ldflag", snippet: "internal/telemetry.BuildSentryDSN=$($env:AUTO_MAS_SENTRY_DSN)"},
		{name: "peeled source commit", snippet: "git rev-parse --verify \"HEAD^{commit}\""},
		{name: "source commit output", snippet: "source_commit=$sourceCommit"},
		{name: "direct executable name", snippet: "$binaryName = \"auto-mas-runtime.exe\""},
		{name: "direct executable checksum", snippet: "Get-FileHash -LiteralPath $binary -Algorithm SHA256"},
		{name: "direct executable upload", snippet: "dist/auto-mas-runtime.exe"},
		{name: "direct executable release", snippet: "release-assets/auto-mas-runtime.exe"},
		{name: "upload action", snippet: "uses: actions/upload-artifact@v7"},
		{name: "download action", snippet: "uses: actions/download-artifact@v8"},
		{name: "release action", snippet: "uses: softprops/action-gh-release@v3"},
		{name: "unmatched asset guard", snippet: "fail_on_unmatched_files: true"},
		{name: "generated notes", snippet: "generate_release_notes: true"},
		{name: "unsigned policy", snippet: "The executable is intentionally unsigned"},
	}
	for _, test := range required {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(source, test.snippet) {
				t.Fatalf("release workflow missing %s snippet %q", test.name, test.snippet)
			}
		})
	}
	if got := strings.Count(source, "contents: write"); got != 1 {
		t.Fatalf("contents: write count = %d, want 1 publish-only permission", got)
	}
	if !strings.Contains(source, "permissions:\n  contents: read") {
		t.Fatal("release workflow missing global read-only permission")
	}
	if strings.Contains(source, "overwrite_files:") {
		t.Fatal("release workflow relies on an undeclared action input for immutability")
	}
	for _, forbidden := range []string{"AUTO_MAS_POSTHOG", "AUTO_MAS_UMAMI", "SENTRY_AUTH_TOKEN"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("release workflow contains cancelled or unnecessary secret %q", forbidden)
		}
	}
	for _, archiveSnippet := range []string{"Compress-Archive", "Expand-Archive", "*.zip", ".zip\""} {
		if strings.Contains(source, archiveSnippet) {
			t.Fatalf("release workflow still packages a zip via %q", archiveSnippet)
		}
	}
	for _, legacy := range []string{
		"uses: actions/upload-artifact@v4",
		"uses: actions/download-artifact@v4",
		"uses: softprops/action-gh-release@v2",
	} {
		if strings.Contains(source, legacy) {
			t.Errorf("release workflow still uses legacy action %q", legacy)
		}
	}
}

func TestReleaseWorkflow_SmokeAndImmutabilityContract(t *testing.T) {
	source := releaseWorkflowSource(t)
	required := []struct {
		name    string
		snippet string
	}{
		{name: "forced tag guard", snippet: "FORCED_PUSH: ${{ github.event.forced }}"},
		{name: "runtime version validation", snippet: "^v[A-Za-z0-9._-]+$"},
		{name: "dot dot guard", snippet: "$env:RELEASE_TAG.Contains(\"..\")"},
		{name: "reflog guard", snippet: "$env:RELEASE_TAG.Contains(\"@{\")"},
		{name: "lock suffix guard", snippet: "$env:RELEASE_TAG.EndsWith(\".lock\")"},
		{name: "prerelease substring", snippet: "$env:RELEASE_TAG -imatch '-(?:beta|rc|alpha)'"},
		{name: "remote tag endpoint", snippet: "/git/ref/tags/$tagPath"},
		{name: "annotated tag endpoint", snippet: "/git/tags/$objectSHA"},
		{name: "tag type dispatch", snippet: "switch ($objectType)"},
		{name: "commit identity check", snippet: "$resolvedCommit -cne $env:SOURCE_COMMIT"},
		{name: "existing release endpoint", snippet: "/releases/tags/$tagPath"},
		{name: "existing release rejection", snippet: "already exists; refusing to overwrite it"},
		{name: "release executable download", snippet: "gh release download $env:RELEASE_TAG"},
		{name: "published executable path", snippet: "BINARY_PATH=$($binaries[0].FullName)"},
		{name: "version NDJSON", snippet: "version --output ndjson"},
		{name: "version empty line rejection", snippet: "version emitted an empty NDJSON line"},
		{name: "version object rejection", snippet: "version emitted a JSON value that is not an object"},
		{name: "version tag identity", snippet: "runtimeVersion -ne $env:RELEASE_TAG"},
		{name: "doctor NDJSON", snippet: "doctor --output ndjson --app-root $smokeRoot"},
		{name: "doctor empty line rejection", snippet: "doctor emitted an empty NDJSON line"},
		{name: "doctor object rejection", snippet: "doctor emitted a JSON value that is not an object"},
		{name: "hello assertion", snippet: "did not start with a hello event"},
		{name: "terminal result assertion", snippet: "did not emit exactly one final result event"},
	}
	for _, test := range required {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(source, test.snippet) {
				t.Fatalf("release workflow missing %s snippet %q", test.name, test.snippet)
			}
		})
	}
	if got := strings.Count(source, "ConvertFrom-Json -ErrorAction Stop -NoEnumerate"); got != 2 {
		t.Fatalf("strict NDJSON parser count = %d, want 2", got)
	}
	if got := strings.Count(source, "[System.Text.Json.JsonDocument]::Parse($line)"); got != 2 {
		t.Fatalf("RFC JSON parser count = %d, want 2", got)
	}
	if got := strings.Count(source, "[System.Text.Json.JsonValueKind]::Object"); got != 2 {
		t.Fatalf("JSON object kind guard count = %d, want 2", got)
	}
	if got := strings.Count(source, "$document.Dispose()"); got != 2 {
		t.Fatalf("JSON document disposal count = %d, want 2", got)
	}
	if got := strings.Count(source, "$event.GetType() -ne [System.Management.Automation.PSCustomObject]"); got != 2 {
		t.Fatalf("NDJSON object guard count = %d, want 2", got)
	}
}

func TestReleaseWorkflow_DirectExecutableContract(t *testing.T) {
	source := releaseWorkflowSource(t)
	for _, want := range []string{
		"$binaryName = \"auto-mas-runtime.exe\"",
		"$hash  $($env:BINARY_NAME)",
		"--pattern $env:BINARY_NAME",
		"& $env:BINARY_PATH version --output ndjson",
		"& $env:BINARY_PATH doctor --output ndjson --app-root $smokeRoot",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("release workflow missing direct executable behavior %q", want)
		}
	}
}

func releaseWorkflowSource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test file")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}
