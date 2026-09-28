package gitrepo

import (
	"strings"
	"testing"

	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"
)

func TestAlphaBinding_RejectsMalformedConfig(t *testing.T) {
	valid := "[auto-mas-runtime]\nversion = v5.6.0-alpha.123\ncommit = " + testGitCommit + "\nsourceversion = v5.6.0\n"
	tests := []struct{ name, config string }{
		{name: "valid", config: valid},
		{name: "missing", config: strings.Replace(valid, "sourceversion = v5.6.0\n", "", 1)},
		{name: "duplicate", config: valid + "version = v5.6.0-alpha.124\n"},
		{name: "extra", config: valid + "extra = ignored\n"},
		{name: "stable target", config: strings.Replace(valid, "v5.6.0-alpha.123", "v5.6.0", 1)},
		{name: "invalid source", config: strings.Replace(valid, "sourceversion = v5.6.0", "sourceversion = dev", 1)},
		{name: "invalid commit", config: strings.Replace(valid, testGitCommit, "not-a-commit", 1)},
		{name: "subsection", config: strings.Replace(valid, "[auto-mas-runtime]", "[auto-mas-runtime \"other\"]", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := gitconfig.New()
			if err := gitconfig.NewDecoder(strings.NewReader(test.config)).Decode(raw); err != nil {
				t.Fatal(err)
			}
			binding, err := readAlphaBinding(raw)
			if test.name == "valid" {
				if err != nil || binding == nil {
					t.Fatalf("binding = %+v, %v", binding, err)
				}
			} else if err == nil {
				t.Fatalf("readAlphaBinding(%s) accepted malformed config", test.name)
			}
		})
	}
}

func TestAlphaIdentity_RejectsMissingBinding(t *testing.T) {
	_, _, snapshot := validVerifierFixture(t)
	snapshot.headTarget = "refs/heads/dev"
	if _, err := repositoryIdentityFromSnapshot(snapshot); err == nil {
		t.Fatal("identity accepted unbound dev")
	}
}
