package gitrepo

import (
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestGoGitCloneOptionsContract(t *testing.T) {
	branch := plumbing.NewBranchReferenceName("release/v5.3.1")
	options := git.CloneOptions{
		URL:           "https://example.com/AUTO-MAS.git",
		ReferenceName: branch,
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("CloneOptions.Validate() error = %v", err)
	}
	if options.ReferenceName != branch {
		t.Fatalf("ReferenceName = %q, want %q", options.ReferenceName, branch)
	}
	if !options.SingleBranch || options.Depth != 1 || options.Tags != git.NoTags {
		t.Fatalf(
			"clone shape = singleBranch:%t depth:%d tags:%d, want true/1/NoTags",
			options.SingleBranch,
			options.Depth,
			options.Tags,
		)
	}
	if options.InsecureSkipTLS {
		t.Fatal("InsecureSkipTLS = true, want false")
	}
	if options.RecurseSubmodules != git.NoRecurseSubmodules {
		t.Fatalf("RecurseSubmodules = %d, want disabled", options.RecurseSubmodules)
	}
}
