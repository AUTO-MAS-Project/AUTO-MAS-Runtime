package gitrepo

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func componentPathIdentity(t *testing.T, path string) componentFileIdentity {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatalf("lstat(%q) = %v", filepath.Base(path), err)
	}
	return componentFileIdentity{volumeSerialNumber: uint32(stat.Dev), fileIndex: stat.Ino}
}

func assertComponentReadOnlyIndex(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("pack index %q mode = %v, want read-only", filepath.Base(path), info.Mode())
	}
}
