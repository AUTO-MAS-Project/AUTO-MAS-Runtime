package macosprobe

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRoot_APFSCapabilities(t *testing.T) {
	dir := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	if got := unix.ByteSliceToString(stat.Fstypename[:]); got != "apfs" {
		t.Fatalf("got filesystem %q, want apfs", got)
	}
	upper := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(upper, []byte("case"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	sensitive := os.IsNotExist(err)
	if err != nil && !sensitive {
		t.Fatal(err)
	}
	if expected := os.Getenv("AUTO_MAS_PROBE_CASE_SENSITIVE"); expected != "" && sensitive != (expected == "true") {
		t.Fatalf("got case-sensitive=%t, want %s", sensitive, expected)
	}
	t.Logf("APFS case-sensitive=%t", sensitive)
}

// TestRoot_PathReplacement 固定重命名顺序，证明 os.Root 追随 FD 而不固定词法位置。
// 这是生产能力不能直接采用字符串 lease.Path 的反例，不是安全写入方案的验收。
func TestRoot_PathReplacement(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "managed")
	moved := filepath.Join(base, "moved")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("fd-only", []byte("authorized inode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "fd-only")); !os.IsNotExist(err) {
		t.Fatalf("got replacement stat %v, want absent", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "fd-only")); err != nil {
		t.Fatalf("got pinned directory stat %v, want existing file", err)
	}
}

// TestRoot_AncestorSymlink 覆盖先验证祖先再将其交换为越界链接的确定性攻击。
func TestRoot_AncestorSymlink(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, "managed")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{managed, outside, filepath.Join(managed, "child")} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(managed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	child := filepath.Join(managed, "child")
	if err := os.Rename(child, filepath.Join(managed, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, child); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("child/sentinel", []byte("modified"), 0o600); err == nil {
		t.Fatal("got successful escaped write, want rejection")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "unchanged" {
		t.Fatalf("got outside sentinel %q, %v, want unchanged", got, err)
	}
	// 根内链接是 os.Root 允许的，不能误当成 Runtime 的任意链接拒绝。
	if err := os.Symlink("original", filepath.Join(managed, "internal-link")); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("internal-link/allowed-by-root", []byte("proof"), 0o600); err != nil {
		t.Fatalf("got %v, want Root to allow contained link", err)
	}
}
