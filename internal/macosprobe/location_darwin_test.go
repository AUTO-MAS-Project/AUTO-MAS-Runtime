package macosprobe

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRoot_IdentityCheckDoesNotPinLocation 复现身份预检和 advisory lock 无法冻结路径位置的反例。
func TestRoot_IdentityCheckDoesNotPinLocation(t *testing.T) {
	base := t.TempDir()
	runtimeDir := filepath.Join(base, "runtime")
	dataDir := filepath.Join(base, "data")
	for _, dir := range []string{runtimeDir, dataDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(dataDir, "keep")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(runtimeDir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(staging)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	lockFile, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lockFile.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	expected, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(staging)
	if err != nil || !os.SameFile(expected, current) {
		t.Fatalf("got precheck %v, want matching identity", err)
	}
	// 固定在最后一次预检之后进行非协作同用户重命名，不依赖调度概率。
	moved := filepath.Join(dataDir, "moved")
	runRootRenamePeer(t, staging, moved)
	if err := root.WriteFile("fd-write", []byte("same authorized inode, new protected location"), 0o600); err != nil {
		t.Fatalf("got FD write %v, want observable Root behavior", err)
	}
	actual, err := os.Stat(moved)
	if err != nil || !os.SameFile(expected, actual) {
		t.Fatalf("got moved identity %v, want original inode", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "fd-write")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "unchanged" {
		t.Fatalf("got unrelated sentinel %q, %v, want unchanged", got, err)
	}
	t.Log("peer renamed a flock-held directory into data after identity precheck; Root followed the same inode")
}
