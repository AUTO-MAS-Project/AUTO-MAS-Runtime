//go:build linux || darwin

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
func TestPOSIXDelete_ProtectUserData(t *testing.T) {
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
	if err := os.MkdirAll(layout.UVCacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(layout.UVCacheDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	request := filesystem.DeleteRequest{Kind: filesystem.DeleteUVCache, Target: layout.UVCacheDir(), OperationID: "op", Reason: "test"}
	if _, err = op.RemoveTree(t.Context(), request); err == nil {
		t.Fatal("got successful link traversal, want rejection")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	result, err := op.RemoveTree(t.Context(), request)
	if err != nil || !result.Removed {
		t.Fatalf("got %+v, %v, want removed", result, err)
	}
	request.Target = layout.DataDir()
	if _, err = op.RemoveTree(t.Context(), request); err == nil {
		t.Fatal("got successful user data deletion, want rejection")
	}
}
