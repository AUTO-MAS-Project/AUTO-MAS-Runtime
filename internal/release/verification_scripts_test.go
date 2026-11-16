package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerificationScripts_CheckFormattingPortably 锁定跨平台且会真实返回失败的格式门禁。
func TestVerificationScripts_CheckFormattingPortably(t *testing.T) {
	root := repositoryRoot(t)
	sources := map[string]string{
		"ci.yml":          filepath.Join(root, ".github", "workflows", "ci.yml"),
		"verify-posix.sh": filepath.Join(root, "scripts", "verify-posix.sh"),
	}
	for name, path := range sources {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		source := string(data)
		for _, required := range []string{"gofmt -w", "git diff --exit-code -- '*.go'"} {
			if !strings.Contains(source, required) {
				t.Errorf("%s missing portable formatting gate %q", name, required)
			}
		}
		for _, forbidden := range []string{"gofmt -l .", "$formatDiff = gofmt"} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s contains non-portable formatting check %q", name, forbidden)
			}
		}
	}
}
