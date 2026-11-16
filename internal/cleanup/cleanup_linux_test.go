//go:build linux

package cleanup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanup_RemovesUVCacheArchiveLinksWithoutFollowingTargets(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)

	archive := filepath.Join(layout.UVCacheDir(), "archive-v0", "package-archive")
	writeFixtureFile(t, filepath.Join(archive, "demo.whl"), []byte("wheel"))

	wheelLink := filepath.Join(layout.UVCacheDir(), "wheels-v5", "pypi", "demo", "1.0.0")
	if err := os.MkdirAll(filepath.Dir(wheelLink), 0o700); err != nil {
		t.Fatal(err)
	}
	archiveTarget, err := filepath.Rel(filepath.Dir(wheelLink), archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(archiveTarget, wheelLink); err != nil {
		t.Fatal(err)
	}

	external := t.TempDir()
	externalSentinel := filepath.Join(external, "keep.txt")
	writeFixtureFile(t, externalSentinel, []byte("keep"))
	externalLink := filepath.Join(layout.UVCacheDir(), "archive-v0", "external-link")
	if err := os.Symlink(external, externalLink); err != nil {
		t.Fatal(err)
	}

	service, _ := newTestService(t, layout)
	report, err := runService(t, service)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if item := findItem(t, report, "uv-cache"); item.Status != ItemCleaned {
		t.Fatalf("uv-cache status = %q, want cleaned", item.Status)
	}
	if dirExists(layout.UVCacheDir()) {
		t.Fatal("uv cache directory still exists")
	}
	if payload, err := os.ReadFile(externalSentinel); err != nil || string(payload) != "keep" {
		t.Fatalf("external sentinel = %q, %v, want unchanged", payload, err)
	}
}
