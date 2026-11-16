//go:build linux || darwin

package gitrepo

import (
	"os"
	"testing"
)

func mustCreateGitRepoJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}
