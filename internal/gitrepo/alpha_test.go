package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestComponent_AlphaDevIdentity(t *testing.T) {
	repository := newGitFixtureRepository(t,
		gitFixtureCommit{label: "old", version: "v5.6.0"},
		gitFixtureCommit{label: "new", version: "v5.7.0-beta.1"},
	)
	branch := plumbing.NewBranchReferenceName("dev")
	if err := repository.repository.Storer.SetReference(plumbing.NewHashReference(branch, repository.hash(t, "old"))); err != nil {
		t.Fatal(err)
	}
	if err := repository.repository.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		t.Fatal(err)
	}
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	layout := componentLayout(t)
	fetcher, _ := componentFetcher(t, layout, server.caBundle)
	target := componentTarget(t, "v5.6.0-alpha.123")
	first, err := fetcher.Fetch(t.Context(), FetchRequest{Plan: plan, Target: target, OperationID: componentOperationID(95)})
	if err != nil {
		t.Fatalf("Fetch(alpha) = %v, want dev clone", err)
	}
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), first.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	// 测试传输使用回环 URL；身份校验仍要求正式目录源白名单。
	snapshot.remotes[0].fetchURLs = []string{recoverySourceURL(t)}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil || identity.version != target.Version() || identity.branch != "dev" || identity.commit != first.Revision.Commit() {
		t.Fatalf("identity = %+v, %v, want alpha/dev/current commit", identity, err)
	}
	payload, err := os.ReadFile(filepath.Join(first.RepositoryPath, "res", "version.json"))
	if err != nil || string(payload) != `{"version":"v5.6.0"}` {
		t.Fatalf("source payload = %s, %v, want unchanged", payload, err)
	}
	if err := repository.repository.Storer.SetReference(plumbing.NewHashReference(branch, repository.hash(t, "new"))); err != nil {
		t.Fatal(err)
	}
	second, err := fetcher.Fetch(t.Context(), FetchRequest{Plan: plan, Target: target, OperationID: componentOperationID(96)})
	if err != nil || second.Revision.Commit() == first.Revision.Commit() {
		t.Fatalf("Fetch(moved dev) = %+v, %v, want new commit", second, err)
	}
	snapshot, err = (goGitRepositoryReader{}).Inspect(t.Context(), second.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.remotes[0].fetchURLs = []string{recoverySourceURL(t)}
	if identity, err := repositoryIdentityFromSnapshot(snapshot); err != nil || identity.version != target.Version() {
		t.Fatalf("next cycle identity = %+v, %v", identity, err)
	}
	cloned, err := git.PlainOpen(second.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := cloned.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.Section("auto-mas-runtime").SetOption("commit", first.Revision.Commit())
	if err := cloned.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snapshot, err = (goGitRepositoryReader{}).Inspect(t.Context(), second.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.remotes[0].fetchURLs = []string{recoverySourceURL(t)}
	if _, err := repositoryIdentityFromSnapshot(snapshot); err == nil {
		t.Fatal("identity accepted mismatched binding commit")
	}
	bindingAttempted := false
	fetcher.bindRevision = func(context.Context, string, Revision) error {
		bindingAttempted = true
		return errors.New("injected alpha binding write failure")
	}
	failedOperation := componentOperationID(97)
	_, err = fetcher.Fetch(t.Context(), FetchRequest{Plan: plan, Target: target, OperationID: failedOperation})
	assertGitrepoCode(t, err, protocol.CodeGitRepositoryInvalid)
	failedPath, pathErr := layout.RepoUpdateDir(failedOperation)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) || !bindingAttempted {
		t.Fatalf("failed alpha binding candidate remains or binding skipped: %v, attempted=%t", err, bindingAttempted)
	}
	server.assertNoServerErrors(t)
}

func bindAlphaFixture(t *testing.T, path, version string) {
	t.Helper()
	repository, err := git.PlainOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := plumbing.NewBranchReferenceName("dev")
	if err := repository.Storer.SetReference(plumbing.NewHashReference(branch, head.Hash())); err != nil {
		t.Fatal(err)
	}
	if err := repository.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		t.Fatal(err)
	}
	revision, err := NewRevision(version, "dev", head.Hash().String(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if err := bindAlphaRevision(t.Context(), path, revision); err != nil {
		t.Fatal(err)
	}
}

func TestService_AlphaStageAndActivateOffline(t *testing.T) {
	s, layout, req, calls := stagingFixtureForVersion(t, "v5.6.0-alpha.123")
	before, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil {
		t.Fatal(err)
	}
	locks, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := locks.Close(); err != nil {
			t.Error(err)
		}
	})
	backend, err := locks.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Stage(t.Context(), req)
	if err != nil || !prepared.Staged {
		t.Fatalf("Stage(alpha) = %+v, %v", prepared, err)
	}
	again, err := s.Stage(t.Context(), req)
	if err != nil || again.Revision != prepared.Revision || *calls != 1 {
		t.Fatalf("Stage repeated = %+v, %v, fetches=%d", again, err, *calls)
	}
	if _, err := s.Sync(t.Context(), req); err == nil {
		t.Fatal("Sync accepted running backend")
	}
	after, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil || string(after) != string(before) {
		t.Fatalf("environment changed during stage: %v", err)
	}
	if err := backend.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	req.Policy, err = mirror.NewPolicy(mirror.PolicySpec{Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	req.OperationID = componentOperationID(71)
	result, err := s.Sync(t.Context(), req)
	if err != nil || !result.Changed || result.Revision != prepared.Revision || *calls != 1 {
		t.Fatalf("offline Sync = %+v, %v, fetches=%d", result, err, *calls)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}

func TestService_AlphaBindingMismatchFailsClosed(t *testing.T) {
	s, layout, req, _ := stagingFixtureForVersion(t, "v5.6.0-alpha.123")
	_, err := s.Stage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	path, err := layout.RepoUpdateDir(req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := git.PlainOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repository.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.Section(alphaConfigSection).SetOption("sourceversion", "v0.0.0")
	if err := repository.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, err = s.Sync(t.Context(), req)
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) || coded.Code() != protocol.CodeUpdateStateAmbiguous {
		t.Fatalf("Sync = %v, want ambiguous", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("invalid prepared repository was removed: %v", err)
	}
}

func TestService_LegacyAlphaReleaseCheck(t *testing.T) {
	layout := componentLayout(t)
	version := "v9.9.9-alpha.1"
	writeRecoveryRepository(t, layout.RepoDir(), version, recoverySourceURL(t), "old")
	s, err := NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Check(t.Context())
	if err != nil || !current.Healthy || current.Version != version || current.Branch != "release/"+version {
		t.Fatalf("legacy alpha Check = %+v, %v", current, err)
	}
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), layout.RepoDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := repositoryIdentityFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRevision(identity.version, identity.branch, identity.commit, identity.sourceKey); err != nil {
		t.Fatal(err)
	}
}

func TestService_LegacyAlphaStagedMigration(t *testing.T) {
	s, layout, req, calls := stagingFixtureWithLegacy(t, "v5.6.0-alpha.123", true)
	current, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	update, err := layout.RepoUpdateDir(req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryRepositoryWithExtraCommit(t, update, req.Target.Version(), recoverySourceURL(t), "new")
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID: req.OperationID, Command: "workspace stage", PID: req.PID,
		TargetVersion: req.Target.Version(), Stage: protocol.StageWorkspaceVerify,
		BaseCommit: current.Commit, TargetCommit: snapshot.commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	req.OperationID = componentOperationID(74)
	result, err := s.Sync(t.Context(), req)
	if err != nil || !result.Changed || result.Revision.Branch() != "dev" || *calls != 1 {
		t.Fatalf("Sync legacy alpha migration = %+v, %v, calls=%d", result, err, *calls)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}
func TestService_AlphaSyncCommitAndBuildIdentity(t *testing.T) {
	s, layout, req, calls := stagingFixtureForVersion(t, "v5.6.0-alpha.123")
	current, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Sync(t.Context(), req)
	if err != nil || !result.Changed || result.Revision.Commit() == current.Commit || *calls != 1 {
		t.Fatalf("Sync moved dev = %+v, %v, calls=%d", result, err, *calls)
	}
	s.resolveRemote = func(_ context.Context, _ mirror.Plan, target Target) (string, error) {
		if target.Branch() != "dev" {
			t.Fatalf("remote branch = %q, want dev", target.Branch())
		}
		return result.Revision.Commit(), nil
	}
	remote, err := s.CheckRemote(t.Context(), req.Policy)
	if err != nil || remote.UpdateAvailable {
		t.Fatalf("CheckRemote unchanged = %+v, %v", remote, err)
	}
	req.OperationID = componentOperationID(72)
	unchanged, err := s.Sync(t.Context(), req)
	if err != nil || unchanged.Changed || *calls != 1 {
		t.Fatalf("Sync unchanged = %+v, %v, calls=%d", unchanged, err, *calls)
	}
	req.Target = componentTarget(t, "v5.6.0-alpha.124")
	req.OperationID = componentOperationID(73)
	next, err := s.Sync(t.Context(), req)
	if err != nil || !next.Changed || next.Revision.Version() != req.Target.Version() ||
		next.Revision.Commit() != result.Revision.Commit() || *calls != 2 {
		t.Fatalf("Sync new build on same commit = %+v, %v, calls=%d", next, err, *calls)
	}
	active, err := s.Check(t.Context())
	if err != nil || !active.Healthy || active.Version != req.Target.Version() || active.Branch != "dev" {
		t.Fatalf("Check new build = %+v, %v", active, err)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}
