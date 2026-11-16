package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyPOSIXScript_RepeatsStateConcurrencyTests 锁定状态事务的并发重复验证。
func TestVerifyPOSIXScript_RepeatsStateConcurrencyTests(t *testing.T) {
	path := filepath.Join(repositoryRoot(t), "scripts", "verify-posix.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	const command = "go test ./internal/protocol ./internal/lock ./internal/state ./internal/process -count=100"
	if !strings.Contains(string(data), command) {
		t.Fatalf("verify-posix.sh missing repeated concurrency command %q", command)
	}
}
