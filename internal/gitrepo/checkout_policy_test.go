package gitrepo

import (
	"errors"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestCheckoutPolicy_DefaultExclusions(t *testing.T) {
	got := repositoryCheckoutExclusions()
	want := []string{"frontend", ".debug"}
	if !slices.Equal(got, want) {
		t.Fatalf("repositoryCheckoutExclusions() = %q, want %q", got, want)
	}
}

func TestCheckoutPolicy_RejectsInvalidExclusions(t *testing.T) {
	tests := []struct {
		name       string
		exclusions []string
	}{
		{name: "empty", exclusions: []string{""}},
		{name: "current directory", exclusions: []string{"."}},
		{name: "parent directory", exclusions: []string{".."}},
		{name: "git directory", exclusions: []string{".git"}},
		{name: "nested slash", exclusions: []string{"frontend/cache"}},
		{name: "nested backslash", exclusions: []string{`frontend\cache`}},
		{name: "wildcard", exclusions: []string{"front*"}},
		{name: "windows absolute", exclusions: []string{`C:\frontend`}},
		{name: "duplicate", exclusions: []string{"frontend", "frontend"}},
		{name: "required file", exclusions: []string{"main.py"}},
		{name: "required directory", exclusions: []string{"app"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newCheckoutPolicy(test.exclusions)
			if !errors.Is(err, errInvalidCheckoutPolicy) {
				t.Fatalf("newCheckoutPolicy(%q) error = %v, want errInvalidCheckoutPolicy", test.exclusions, err)
			}
		})
	}
}

func TestCheckoutPolicy_RootEntries(t *testing.T) {
	policy, err := newCheckoutPolicy([]string{"frontend", ".debug"})
	if err != nil {
		t.Fatalf("newCheckoutPolicy() error = %v", err)
	}
	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: "res", Mode: filemode.Dir},
		{Name: "frontend", Mode: filemode.Dir},
		{Name: "main.py", Mode: filemode.Regular},
		{Name: ".debug", Mode: filemode.Regular},
		{Name: "frontend-tools", Mode: filemode.Dir},
		{Name: "app", Mode: filemode.Dir},
	}}
	got, err := policy.patterns(tree)
	if err != nil {
		t.Fatalf("patterns() error = %v", err)
	}
	want := []string{"app/", "frontend-tools/", "main.py", "res/"}
	if !slices.Equal(got, want) {
		t.Fatalf("patterns() = %q, want %q", got, want)
	}
}

func TestCheckoutPolicy_RejectsPrefixAmbiguity(t *testing.T) {
	policy, err := newCheckoutPolicy([]string{"frontend"})
	if err != nil {
		t.Fatalf("newCheckoutPolicy() error = %v", err)
	}
	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: "front", Mode: filemode.Regular},
		{Name: "frontend", Mode: filemode.Dir},
	}}
	_, err = policy.patterns(tree)
	if !errors.Is(err, errInvalidCheckoutPolicy) {
		t.Fatalf("patterns() error = %v, want errInvalidCheckoutPolicy", err)
	}
}
