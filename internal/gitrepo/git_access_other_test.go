//go:build !darwin

package gitrepo

import "testing"

func testRepositoryAccess(t *testing.T, name string) repositoryAccess {
	t.Helper()
	return repositoryAccess{path: name}
}
