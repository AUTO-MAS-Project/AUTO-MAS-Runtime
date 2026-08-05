package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestVerifier_AcceptsVerifiedShallowRepository(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0-beta.4")
	plan := mustGitPlan(t, "cnb")
	source := plan.Sources()[0]
	repositoryPath, commit := createVerifiedRepository(t, target.Version(), source.BaseURL())
	verifier := NewVerifier()

	revision, err := verifier.Verify(t.Context(), VerificationRequest{
		RepositoryPath: repositoryPath,
		Target:         target,
		Source:         source,
		AllowedSources: plan.Sources(),
	})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if revision.Version() != target.Version() ||
		revision.Branch() != target.Branch() ||
		revision.Commit() != commit ||
		revision.SourceKey() != source.Key() {
		t.Fatalf("Verify() revision = %#v, want target/commit/source", revision)
	}
}

func TestVerifier_RejectsUnknownRemoteAndWrongBranch(t *testing.T) {
	_, request, snapshot := validVerifierFixture(t)
	tests := []struct {
		name   string
		mutate func(*VerificationRequest, *repositorySnapshot)
	}{
		{name: "remote name", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.remotes[0].name = "upstream"
		}},
		{name: "unknown URL", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.remotes[0].fetchURLs[0] = "https://unknown.example/repo.git"
		}},
		{name: "multiple fetch URLs", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.remotes[0].fetchURLs = append(snapshot.remotes[0].fetchURLs, "https://other.example/repo.git")
		}},
		{name: "mirror remote", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.remotes[0].mirror = true
		}},
		{name: "multiple remotes", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.remotes = append(snapshot.remotes, remoteSnapshot{name: "other", fetchURLs: []string{"https://other.example/repo.git"}})
		}},
		{name: "wrong branch", mutate: func(_ *VerificationRequest, snapshot *repositorySnapshot) {
			snapshot.headTarget = "refs/heads/release/v0.0.0"
		}},
		{name: "source outside plan", mutate: func(request *VerificationRequest, _ *repositorySnapshot) {
			request.AllowedSources = request.AllowedSources[1:]
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			currentRequest := cloneVerificationRequestForTest(request)
			currentSnapshot := cloneRepositorySnapshot(snapshot)
			tt.mutate(&currentRequest, &currentSnapshot)
			verifier := mustVerifierWithSnapshot(t, currentSnapshot, nil)
			revision, err := verifier.Verify(t.Context(), currentRequest)
			if revision != (Revision{}) {
				t.Fatalf("Verify() revision = %#v, want zero", revision)
			}
			assertGitrepoCode(t, err, protocol.CodeGitRepositoryInvalid)
		})
	}
}

func TestVerifier_RejectsInvalidRepositoryShapes(t *testing.T) {
	_, request, snapshot := validVerifierFixture(t)
	tests := []struct {
		name      string
		mutate    func(*repositorySnapshot)
		readerErr error
	}{
		{name: "open failure", readerErr: errors.New("test repository open failure")},
		{name: "bare", mutate: func(snapshot *repositorySnapshot) { snapshot.nonBare = false }},
		{name: "detached HEAD", mutate: func(snapshot *repositorySnapshot) { snapshot.headSymbolic = false }},
		{name: "invalid commit", mutate: func(snapshot *repositorySnapshot) { snapshot.commit = "ABC" }},
		{name: "missing shallow boundary", mutate: func(snapshot *repositorySnapshot) { snapshot.shallow = nil }},
		{name: "wrong shallow boundary", mutate: func(snapshot *repositorySnapshot) {
			snapshot.shallow = []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		}},
		{name: "tag present", mutate: func(snapshot *repositorySnapshot) { snapshot.tags = []string{"refs/tags/v5.4.0"} }},
		{name: "version symlink", mutate: func(snapshot *repositorySnapshot) { snapshot.versionMode = filemode.Symlink }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := cloneRepositorySnapshot(snapshot)
			if tt.mutate != nil {
				tt.mutate(&current)
			}
			verifier := mustVerifierWithSnapshot(t, current, tt.readerErr)
			revision, err := verifier.Verify(t.Context(), request)
			if revision != (Revision{}) {
				t.Fatalf("Verify() revision = %#v, want zero", revision)
			}
			assertGitrepoCode(t, err, protocol.CodeGitRepositoryInvalid)
		})
	}
}

func TestVerifier_RejectsMissingDuplicateAndMalformedVersion(t *testing.T) {
	_, request, snapshot := validVerifierFixture(t)
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "missing", payload: []byte(`{"other":true}`)},
		{name: "duplicate", payload: []byte(`{"version":"v5.4.0","version":"v5.4.0"}`)},
		{name: "non string", payload: []byte(`{"version":42}`)},
		{name: "null", payload: []byte(`{"version":null}`)},
		{name: "invalid JSON", payload: []byte(`{"version":`)},
		{name: "top level array", payload: []byte(`[{"version":"v5.4.0"}]`)},
		{name: "trailing value", payload: []byte(`{"version":"v5.4.0"} true`)},
		{name: "invalid UTF-8", payload: []byte{'{', '"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '"', 0xff, '"', '}'}},
		{name: "too large", payload: bytes.Repeat([]byte(" "), maxRepositoryVersionBytes+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := cloneRepositorySnapshot(snapshot)
			current.versionPayload = tt.payload
			verifier := mustVerifierWithSnapshot(t, current, nil)
			revision, err := verifier.Verify(t.Context(), request)
			if revision != (Revision{}) {
				t.Fatalf("Verify() revision = %#v, want zero", revision)
			}
			assertGitrepoCode(t, err, protocol.CodeGitRepositoryInvalid)
		})
	}
}

func TestVerifier_MapsVersionMismatch(t *testing.T) {
	_, request, snapshot := validVerifierFixture(t)
	snapshot.versionPayload = []byte(`{"version":"v5.3.0"}`)
	verifier := mustVerifierWithSnapshot(t, snapshot, nil)

	revision, err := verifier.Verify(t.Context(), request)
	if revision != (Revision{}) {
		t.Fatalf("Verify() revision = %#v, want zero", revision)
	}
	assertGitrepoCode(t, err, protocol.CodeGitVersionMismatch)
}

func TestFetcher_VerificationFailureRotatesAndPreservesActiveRepository(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	plan := mustGitPlan(t, "cnb")
	layout := mustGitLayout(t)
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("MkdirAll(active repo) error = %v", err)
	}
	activeMarker := filepath.Join(layout.RepoDir(), "active-marker")
	if err := os.WriteFile(activeMarker, []byte("active"), 0o600); err != nil {
		t.Fatalf("WriteFile(active marker) error = %v", err)
	}
	client := &fakeGitClient{
		list: func(_ context.Context, _ string, _ []byte) ([]*plumbing.Reference, error) {
			return targetBranchReferences(target), nil
		},
		clone: func(_ context.Context, path string, options git.CloneOptions) error {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "source"), []byte(options.URL), 0o600)
		},
	}
	verifier := &fakeCloneVerifier{
		verify: func(_ context.Context, request VerificationRequest) (Revision, error) {
			if request.Source.Key() == "cnb" {
				return Revision{}, newError(
					protocol.CodeGitRepositoryInvalid,
					protocol.StageWorkspaceVerify,
					messageForCode(protocol.CodeGitRepositoryInvalid),
					map[string]any{},
					errors.New("test integrity failure"),
				)
			}
			return newRevision(request.Target, testGitCommit, request.Source)
		},
	}
	cleanupCount := 0
	remover := &fakeTreeRemover{
		remove: func(_ context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			cleanupCount++
			if err := os.Remove(filepath.Join(request.Target, "source")); err != nil {
				return filesystem.DeleteResult{}, err
			}
			if err := os.Remove(request.Target); err != nil {
				return filesystem.DeleteResult{}, err
			}
			return filesystem.DeleteResult{Removed: true, AuditCompleted: true}, nil
		},
	}
	fetcher := mustTestFetcher(t, fetcherDependencies{
		layout:       layout,
		rotator:      mustTestRotator(t),
		git:          client,
		remover:      remover,
		verifier:     verifier,
		emitProgress: (&progressRecorder{}).EmitProgress,
	})

	result, err := fetcher.Fetch(t.Context(), FetchRequest{
		Plan:        plan,
		Target:      target,
		OperationID: "OPERATION-VERIFY",
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result.Revision.SourceKey() != "github" || cleanupCount != 1 {
		t.Fatalf("Fetch() source/cleanup = %q/%d, want github/1", result.Revision.SourceKey(), cleanupCount)
	}
	if got, err := os.ReadFile(activeMarker); err != nil || string(got) != "active" {
		t.Fatalf("active marker = %q, %v; want preserved", got, err)
	}
	updatePath, err := layout.RepoUpdateDir("OPERATION-VERIFY")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(updatePath, "source")); err != nil || !bytes.Contains(got, []byte("github.com")) {
		t.Fatalf("verified update source = %q, %v; want github", got, err)
	}
}

type fakeRepositoryReader struct {
	snapshot repositorySnapshot
	err      error
}

func (r *fakeRepositoryReader) Inspect(context.Context, string) (repositorySnapshot, error) {
	return cloneRepositorySnapshot(r.snapshot), r.err
}

func validVerifierFixture(t *testing.T) (Target, VerificationRequest, repositorySnapshot) {
	t.Helper()
	target := mustParseTarget(t, "v5.4.0")
	plan := mustGitPlan(t, "cnb")
	source := plan.Sources()[0]
	request := VerificationRequest{
		RepositoryPath: `C:\managed\repo.update-test`,
		Target:         target,
		Source:         source,
		AllowedSources: plan.Sources(),
	}
	snapshot := repositorySnapshot{
		nonBare:      true,
		remotes:      []remoteSnapshot{{name: "origin", fetchURLs: []string{source.BaseURL()}}},
		headSymbolic: true,
		headTarget:   "refs/heads/" + target.Branch(),
		commit:       testGitCommit,
		shallow:      []string{testGitCommit},
		versionMode:  filemode.Regular,
		versionPayload: []byte(
			`{"version":"v5.4.0"}`,
		),
	}
	return target, request, snapshot
}

func mustVerifierWithSnapshot(t *testing.T, snapshot repositorySnapshot, err error) *Verifier {
	t.Helper()
	verifier, constructorErr := newVerifierWithReader(&fakeRepositoryReader{snapshot: snapshot, err: err})
	if constructorErr != nil {
		t.Fatalf("newVerifierWithReader() error = %v", constructorErr)
	}
	return verifier
}

func cloneRepositorySnapshot(snapshot repositorySnapshot) repositorySnapshot {
	cloned := snapshot
	cloned.remotes = make([]remoteSnapshot, len(snapshot.remotes))
	for i, remote := range snapshot.remotes {
		cloned.remotes[i] = remoteSnapshot{
			name:      remote.name,
			fetchURLs: append([]string(nil), remote.fetchURLs...),
			mirror:    remote.mirror,
		}
	}
	cloned.shallow = append([]string(nil), snapshot.shallow...)
	cloned.tags = append([]string(nil), snapshot.tags...)
	cloned.versionPayload = append([]byte(nil), snapshot.versionPayload...)
	return cloned
}

func cloneVerificationRequestForTest(request VerificationRequest) VerificationRequest {
	request.AllowedSources = append([]mirror.Source(nil), request.AllowedSources...)
	return request
}
