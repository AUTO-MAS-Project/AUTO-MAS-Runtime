package gitrepo

import (
	"context"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func inspectTestRepository(t *testing.T, name string) (repositorySnapshot, error) {
	t.Helper()
	return (goGitRepositoryReader{}).Inspect(t.Context(), testRepositoryAccess(t, name))
}

func restoreTestRepository(ctx context.Context, t *testing.T, layout *config.Layout, commit string, beforeWrite func() error) (int, error) {
	t.Helper()
	return (goGitRepositoryReader{}).Restore(ctx, layout, testRepositoryAccess(t, layout.RepoDir()).lease, commit, beforeWrite)
}
