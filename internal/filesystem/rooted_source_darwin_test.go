package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinRootedSource_PublishExecutable(t *testing.T) {
	layout := darwinTestLayout(t)
	lease, err := EnsureManagedDirectory(t.Context(), layout, layout.RepoDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	root, err := lease.RootedDirectory()
	if err != nil {
		t.Fatal(err)
	}
	const name = "app/tool"
	if _, id, err := root.ReadSource(t.Context(), name, 32); err != nil || id != nil {
		t.Fatalf("ReadSource(missing parent) = %v, %v, want missing", id, err)
	}
	if result, err := root.ReplaceSource(t.Context(), name, []byte("executable"), 0o755, nil); err != nil || !result.Published {
		t.Fatalf("ReplaceSource = %#v, %v, want published", result, err)
	}
	info, err := os.Stat(filepath.Join(layout.RepoDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("published mode = %o, want 755", info.Mode().Perm())
	}
	if payload, id, err := root.ReadSource(t.Context(), name, 4); err != nil || id == nil || string(payload) != "exec" {
		t.Fatalf("ReadSource = %q, %v, %v, want limited content with identity", payload, id, err)
	}
}
