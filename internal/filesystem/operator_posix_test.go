//go:build linux

package filesystem_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

type posixAuditor struct {
	records []filesystem.DeleteAuditRecord
}

func (a *posixAuditor) RecordDeletion(_ context.Context, r filesystem.DeleteAuditRecord) error {
	a.records = append(a.records, r)
	return nil
}
func TestPOSIXDelete_RejectsRootSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	audit := &posixAuditor{}
	op, err := filesystem.New(t.Context(), layout, audit)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.UVCacheDir()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.UVCacheDir()); err != nil {
		t.Fatal(err)
	}
	request := filesystem.DeleteRequest{Kind: filesystem.DeleteUVCache, Target: layout.UVCacheDir(), OperationID: "op", Reason: "test"}
	if _, err = op.RemoveTree(t.Context(), request); err == nil {
		t.Fatal("got successful root link replacement, want rejection")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}
	if info, err := os.Lstat(layout.UVCacheDir()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("root link = %v, %v, want preserved", info, err)
	}
	request.Target = layout.DataDir()
	if _, err = op.RemoveTree(t.Context(), request); err == nil {
		t.Fatal("got successful user data deletion, want rejection")
	}
}

func TestPOSIXDelete_OnlyUVCacheAllowsLeafSymlinks(t *testing.T) {
	layout, err := config.NewLayout(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	op, err := filesystem.New(t.Context(), layout, &posixAuditor{})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(layout.UVCacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	cacheLink := filepath.Join(layout.UVCacheDir(), "archive-link")
	if err := os.Symlink(outside, cacheLink); err != nil {
		t.Fatal(err)
	}
	result, err := op.RemoveTree(t.Context(), filesystem.DeleteRequest{
		Kind: filesystem.DeleteUVCache, Target: layout.UVCacheDir(), OperationID: "uv-cache", Reason: "test",
	})
	if err != nil || !result.Removed {
		t.Fatalf("RemoveTree(UVCache) = %+v, %v, want removed", result, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("external target changed: %q, %v", data, err)
	}

	if err := os.MkdirAll(layout.BuildCacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	buildLink := filepath.Join(layout.BuildCacheDir(), "archive-link")
	if err := os.Symlink(outside, buildLink); err != nil {
		t.Fatal(err)
	}
	if _, err := op.RemoveTree(t.Context(), filesystem.DeleteRequest{
		Kind: filesystem.DeleteBuildCache, Target: layout.BuildCacheDir(), OperationID: "build-cache", Reason: "test",
	}); err == nil {
		t.Fatal("got successful build cache leaf link removal, want rejection")
	}
	if info, err := os.Lstat(buildLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("build cache link = %v, %v, want preserved", info, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("external target changed after rejection: %q, %v", data, err)
	}
}
