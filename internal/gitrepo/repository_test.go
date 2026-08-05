package gitrepo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestRepositoryReader_InspectsCommittedIdentity(t *testing.T) {
	version := "v5.4.0-beta.4"
	sourceURL := "https://example.test/AUTO-MAS.git"
	repositoryPath, commit := createVerifiedRepository(t, version, sourceURL)

	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), repositoryPath)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !snapshot.nonBare {
		t.Fatal("snapshot.nonBare = false, want true")
	}
	if len(snapshot.remotes) != 1 ||
		snapshot.remotes[0].name != "origin" ||
		len(snapshot.remotes[0].fetchURLs) != 1 ||
		snapshot.remotes[0].fetchURLs[0] != sourceURL {
		t.Fatalf("snapshot.remotes = %#v, want one origin source", snapshot.remotes)
	}
	if !snapshot.headSymbolic || snapshot.headTarget != plumbing.NewBranchReferenceName("release/"+version).String() {
		t.Fatalf("HEAD = symbolic:%t target:%q, want target release branch", snapshot.headSymbolic, snapshot.headTarget)
	}
	if snapshot.commit != commit {
		t.Fatalf("commit = %q, want %q", snapshot.commit, commit)
	}
	if len(snapshot.shallow) != 1 || snapshot.shallow[0] != commit {
		t.Fatalf("shallow = %#v, want HEAD only", snapshot.shallow)
	}
	if len(snapshot.tags) != 0 {
		t.Fatalf("tags = %#v, want none", snapshot.tags)
	}
	if !snapshot.versionMode.IsRegular() || string(snapshot.versionPayload) != `{"version":"v5.4.0-beta.4"}` {
		t.Fatalf("version file = mode:%v payload:%q, want regular expected JSON", snapshot.versionMode, snapshot.versionPayload)
	}
}

func TestRevision_ValuesAreImmutable(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	source := mustGitPlan(t, "cnb").Sources()[0]
	revision, err := newRevision(target, testGitCommit, source)
	if err != nil {
		t.Fatalf("newRevision() error = %v", err)
	}
	if revision.Version() != target.Version() ||
		revision.Branch() != target.Branch() ||
		revision.Commit() != testGitCommit ||
		revision.SourceKey() != source.Key() {
		t.Fatalf("revision = %#v, want target/commit/source", revision)
	}
	revisionType := reflect.TypeFor[Revision]()
	for i := 0; i < revisionType.NumField(); i++ {
		if field := revisionType.Field(i); field.IsExported() {
			t.Fatalf("Revision field %q is exported", field.Name)
		}
	}
	for _, invalid := range []Revision{
		{},
		{version: target.Version(), branch: target.Branch(), commit: "bad", sourceKey: source.Key()},
		{version: target.Version(), branch: "release/v0", commit: testGitCommit, sourceKey: source.Key()},
	} {
		if err := invalid.validate(); err == nil {
			t.Fatalf("Revision %#v validate() = nil, want error", invalid)
		}
	}
}

func createVerifiedRepository(t *testing.T, version, sourceURL string) (string, string) {
	t.Helper()
	repositoryPath := filepath.Join(t.TempDir(), "repo")
	repository, err := git.PlainInit(repositoryPath, false)
	if err != nil {
		t.Fatalf("PlainInit() error = %v", err)
	}
	versionPath := filepath.Join(repositoryPath, "res", "version.json")
	if err := os.MkdirAll(filepath.Dir(versionPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(versionPath, []byte(`{"version":"`+version+`"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if _, err := worktree.Add("res/version.json"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	hash, err := worktree.Commit("test release", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "AUTO-MAS test",
			Email: "test@example.invalid",
			When:  time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	oldHead, err := repository.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	branch := plumbing.NewBranchReferenceName("release/" + version)
	if err := repository.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
		t.Fatalf("SetReference(branch) error = %v", err)
	}
	if err := repository.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		t.Fatalf("SetReference(HEAD) error = %v", err)
	}
	if oldHead.Name() != branch {
		if err := repository.Storer.RemoveReference(oldHead.Name()); err != nil {
			t.Fatalf("RemoveReference(%q) error = %v", oldHead.Name(), err)
		}
	}
	if _, err := repository.CreateRemote(&gitcfg.RemoteConfig{
		Name: "origin",
		URLs: []string{sourceURL},
	}); err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}
	if err := repository.Storer.SetShallow([]plumbing.Hash{hash}); err != nil {
		t.Fatalf("SetShallow() error = %v", err)
	}
	return repositoryPath, hash.String()
}
