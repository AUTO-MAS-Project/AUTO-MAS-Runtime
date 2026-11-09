package gitrepo

import (
	"os"
	"testing"
)

func mustCreateGitRepoDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
