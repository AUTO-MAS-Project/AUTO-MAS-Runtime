package gitrepo

import (
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func testRepositoryAccess(t *testing.T, name string) repositoryAccess {
	t.Helper()
	parent := filepath.Dir(name)
	layout, err := config.NewLayout(parent, parent)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := filesystem.OpenManagedDirectory(t.Context(), layout, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return repositoryAccess{path: name, lease: lease}
}
