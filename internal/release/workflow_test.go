// Package release 包含 GitHub Release workflow 的静态契约测试。
package release

import (
	"archive/zip"
	"bytes"
	"io"
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
		{name: "license guard", snippet: "LICENSE is required for release packaging"},
		{name: "archive entries", snippet: "Compress-Archive -Path @(\"auto-mas-runtime.exe\", \"LICENSE\", \"README.md\")"},
		{name: "SHA256 sums", snippet: "Get-FileHash -LiteralPath $archive -Algorithm SHA256"},
		{name: "release action", snippet: "uses: softprops/action-gh-release@v2"},
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
		{name: "release archive download", snippet: "gh release download $env:RELEASE_TAG"},
		{name: "version NDJSON", snippet: "version --output ndjson"},
		{name: "version tag identity", snippet: "runtimeVersion -ne $env:RELEASE_TAG"},
		{name: "doctor NDJSON", snippet: "doctor --output ndjson --app-root $smokeRoot"},
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
}

func TestReleaseWorkflow_ReadmeArchiveContract(t *testing.T) {
	source := releaseWorkflowSource(t)
	template := releaseReadmeTemplate(t, source)
	if strings.Contains(template, "`") {
		t.Fatal("release README template contains a PowerShell escape character")
	}
	readme := strings.ReplaceAll(template, "{0}", "v5.2.0-withplugin.0.0.1")
	for _, want := range []string{
		"AUTO-MAS Runtime v5.2.0-withplugin.0.0.1",
		"auto-mas-runtime.exe version",
		"auto-mas-runtime.exe doctor --output ndjson",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("release README = %q, want text %q", readme, want)
		}
	}
	for _, character := range readme {
		if character < 0x20 && character != '\n' && character != '\r' && character != '\t' {
			t.Fatalf("release README contains C0 control U+%04X", character)
		}
	}

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, file := range []struct {
		name     string
		contents string
	}{
		{name: "auto-mas-runtime.exe", contents: "binary"},
		{name: "LICENSE", contents: "license"},
		{name: "README.md", contents: readme},
	} {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", file.name, err)
		}
		if _, err := io.WriteString(entry, file.contents); err != nil {
			t.Fatalf("WriteString(%q) error = %v", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Writer.Close() error = %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	if len(reader.File) != 3 {
		t.Fatalf("archive entries = %d, want 3", len(reader.File))
	}
	for _, file := range reader.File {
		if file.Name != "README.md" {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatalf("README Open() error = %v", err)
		}
		contents, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("README read error = %v, close error = %v", readErr, closeErr)
		}
		if got := string(contents); got != readme {
			t.Fatalf("archived README = %q, want %q", got, readme)
		}
		return
	}
	t.Fatal("archive does not contain README.md")
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

func releaseReadmeTemplate(t *testing.T, source string) string {
	t.Helper()
	const startMarker = "$readme = @'\n"
	const endMarker = "\n          '@ -f $env:RELEASE_TAG"
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("release workflow missing README start marker %q", startMarker)
	}
	start += len(startMarker)
	end := strings.Index(source[start:], endMarker)
	if end < 0 {
		t.Fatalf("release workflow missing README end marker %q", endMarker)
	}
	lines := strings.Split(source[start:start+end], "\n")
	for index, line := range lines {
		lines[index] = strings.TrimPrefix(line, "          ")
	}
	return strings.Join(lines, "\n")
}
