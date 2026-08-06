package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const testGitCommit = "0123456789abcdef0123456789abcdef01234567"

var (
	errTestResolve = errors.New("test resolve failure")
	errTestClone   = errors.New("test clone failure")
)

func TestCloneStorage_DoesNotExposePackfileWriter(t *testing.T) {
	called := false
	store := cloneStorage{init: func() error {
		called = true
		return nil
	}}
	if err := store.Init(); err != nil || !called {
		t.Fatalf("cloneStorage.Init() = %v, called = %t, want forwarded init", err, called)
	}
	if _, ok := any(store).(storer.PackfileWriter); ok {
		t.Fatal("cloneStorage exposes PackfileWriter; truncated packs could leak tmp_pack handles")
	}
	if _, ok := any(&store).(storer.PackfileWriter); ok {
		t.Fatal("*cloneStorage exposes PackfileWriter; truncated packs could leak tmp_pack handles")
	}
}

func TestFetcher_CloneSingleBranchDepthOneWithoutTags(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0-beta.4")
	plan := mustGitPlan(t, "cnb")
	layout := mustGitLayout(t)
	caBundle := []byte("test-ca-bundle")
	progress := &progressRecorder{}

	client := &fakeGitClient{
		list: func(_ context.Context, sourceURL string, gotCA []byte) ([]*plumbing.Reference, error) {
			if sourceURL != "https://cnb.cool/AUTO-MAS-Project/AUTO-MAS.git" {
				t.Fatalf("ListReferences() URL = %q, want cnb source", sourceURL)
			}
			if string(gotCA) != string(caBundle) {
				t.Fatalf("ListReferences() CA = %q, want %q", gotCA, caBundle)
			}
			return targetBranchReferences(target), nil
		},
		clone: func(_ context.Context, path string, options git.CloneOptions) error {
			wantPath, err := layout.RepoUpdateDir("OPERATION-1")
			if err != nil {
				t.Fatalf("RepoUpdateDir() error = %v", err)
			}
			if path != wantPath {
				t.Fatalf("Clone() path = %q, want %q", path, wantPath)
			}
			if options.URL != "https://cnb.cool/AUTO-MAS-Project/AUTO-MAS.git" {
				t.Fatalf("CloneOptions.URL = %q, want cnb source", options.URL)
			}
			if got, want := options.ReferenceName, plumbing.NewBranchReferenceName(target.Branch()); got != want {
				t.Fatalf("ReferenceName = %q, want %q", got, want)
			}
			if !options.SingleBranch || options.Depth != 1 || options.Tags != git.NoTags {
				t.Fatalf("clone shape = single:%t depth:%d tags:%d, want true/1/NoTags", options.SingleBranch, options.Depth, options.Tags)
			}
			if options.RemoteName != "origin" || options.RecurseSubmodules != git.NoRecurseSubmodules {
				t.Fatalf("clone remote/submodules = %q/%d, want origin/disabled", options.RemoteName, options.RecurseSubmodules)
			}
			if options.InsecureSkipTLS {
				t.Fatal("InsecureSkipTLS = true, want false")
			}
			if string(options.CABundle) != string(caBundle) {
				t.Fatalf("CloneOptions.CABundle = %q, want %q", options.CABundle, caBundle)
			}
			if options.Progress == nil {
				t.Fatal("CloneOptions.Progress = nil, want controlled writer")
			}
			if _, err := options.Progress.Write([]byte("remote: secret natural-language progress\n")); err != nil {
				t.Fatalf("Progress.Write() error = %v", err)
			}
			return nil
		},
	}
	verifier := &fakeCloneVerifier{
		verify: func(_ context.Context, request VerificationRequest) (Revision, error) {
			if request.Target != target || request.Source.Key() != "cnb" {
				t.Fatalf("Verify() target/source = %#v/%q, want target/cnb", request.Target, request.Source.Key())
			}
			if len(request.AllowedSources) != 2 {
				t.Fatalf("Verify() allowed sources = %d, want 2", len(request.AllowedSources))
			}
			return newRevision(request.Target, testGitCommit, request.Source)
		},
	}
	remover := &fakeTreeRemover{
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called on successful fetch")
			return filesystem.DeleteResult{}, nil
		},
	}
	fetcher := mustTestFetcher(t, fetcherDependencies{
		layout:       layout,
		rotator:      mustTestRotator(t),
		git:          client,
		remover:      remover,
		verifier:     verifier,
		emitProgress: progress.EmitProgress,
		caBundle:     caBundle,
	})

	var stages []protocol.Stage
	result, err := fetcher.Fetch(t.Context(), FetchRequest{
		Plan:        plan,
		Target:      target,
		OperationID: "OPERATION-1",
		StageReporter: func(stage protocol.Stage) {
			stages = append(stages, stage)
		},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result.Revision.Commit() != testGitCommit || result.Revision.SourceKey() != "cnb" {
		t.Fatalf("Fetch() result = %#v, want commit/cnb", result)
	}
	wantPath, err := layout.RepoUpdateDir("OPERATION-1")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if result.RepositoryPath != wantPath {
		t.Fatalf("RepositoryPath = %q, want %q", result.RepositoryPath, wantPath)
	}
	if !containsStageSequence(stages, protocol.StageWorkspaceClone, protocol.StageWorkspaceVerify) {
		t.Fatalf("reported stages = %v, want clone/verify", stages)
	}
	events := progress.Events()
	if len(events) < 3 {
		t.Fatalf("progress event count = %d, want at least start/pulse/success", len(events))
	}
	if got := events[len(events)-1].Status; got != protocol.ProgressSucceeded {
		t.Fatalf("last progress status = %q, want succeeded", got)
	}
	for _, event := range events {
		if strings.Contains(event.Message, "secret") || strings.Contains(event.Message, "remote:") {
			t.Fatalf("progress message exposes Git text: %q", event.Message)
		}
	}
}

func TestFetcher_PreCancelledPathInspectionReturnsCancelled(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	layout := mustGitLayout(t)
	update, err := layout.RepoUpdateDir("OPERATION-PRE-CANCEL")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if err := os.MkdirAll(update, 0o700); err != nil {
		t.Fatalf("MkdirAll(update) error = %v", err)
	}
	clientCalled := false
	client := &fakeGitClient{
		list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
			clientCalled = true
			return nil, nil
		},
		clone: func(context.Context, string, git.CloneOptions) error {
			clientCalled = true
			return nil
		},
	}
	dependencies := successfulFetcherDependencies(t, client)
	dependencies.layout = layout
	fetcher := mustTestFetcher(t, dependencies)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = fetcher.Fetch(ctx, FetchRequest{
		Plan:        mustGitPlan(t, "cnb"),
		Target:      target,
		OperationID: "OPERATION-PRE-CANCEL",
	})
	assertGitrepoCode(t, err, protocol.CodeOperationCancelled)
	if clientCalled {
		t.Fatal("Git client called for pre-cancelled fetch")
	}
}

func TestFetcher_RotatesWhenPreferredSourceLacksBranch(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	plan := mustGitPlan(t, "cnb")
	var listed []string
	var cloned []string
	client := &fakeGitClient{
		list: func(_ context.Context, sourceURL string, _ []byte) ([]*plumbing.Reference, error) {
			listed = append(listed, sourceURL)
			if strings.Contains(sourceURL, "cnb.cool") {
				return []*plumbing.Reference{
					plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(testGitCommit)),
				}, nil
			}
			return targetBranchReferences(target), nil
		},
		clone: func(_ context.Context, _ string, options git.CloneOptions) error {
			cloned = append(cloned, options.URL)
			return nil
		},
	}
	fetcher := mustTestFetcher(t, successfulFetcherDependencies(t, client))

	result, err := fetcher.Fetch(t.Context(), FetchRequest{Plan: plan, Target: target, OperationID: "OPERATION-2"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result.Revision.SourceKey() != "github" {
		t.Fatalf("SourceKey = %q, want github", result.Revision.SourceKey())
	}
	if len(listed) != 2 || !strings.Contains(listed[0], "cnb.cool") || !strings.Contains(listed[1], "github.com") {
		t.Fatalf("listed sources = %#v, want cnb then github", listed)
	}
	if len(cloned) != 1 || !strings.Contains(cloned[0], "github.com") {
		t.Fatalf("cloned sources = %#v, want only github", cloned)
	}
}

func TestFetcher_MapsResolveCloneAndBranchFailures(t *testing.T) {
	tests := []struct {
		name     string
		list     func(Target, string) ([]*plumbing.Reference, error)
		cloneErr error
		wantCode protocol.Code
	}{
		{
			name: "resolve",
			list: func(_ Target, _ string) ([]*plumbing.Reference, error) {
				return nil, errTestResolve
			},
			wantCode: protocol.CodeGitRemoteResolveFailed,
		},
		{
			name: "branch missing",
			list: func(_ Target, _ string) ([]*plumbing.Reference, error) {
				return nil, nil
			},
			wantCode: protocol.CodeGitBranchNotFound,
		},
		{
			name: "clone",
			list: func(target Target, _ string) ([]*plumbing.Reference, error) {
				return targetBranchReferences(target), nil
			},
			cloneErr: errTestClone,
			wantCode: protocol.CodeGitCloneFailed,
		},
		{
			name: "resolve outranks branch missing",
			list: func(_ Target, sourceURL string) ([]*plumbing.Reference, error) {
				if strings.Contains(sourceURL, "cnb.cool") {
					return nil, nil
				}
				return nil, errTestResolve
			},
			wantCode: protocol.CodeGitRemoteResolveFailed,
		},
		{
			name: "clone outranks resolve",
			list: func(target Target, sourceURL string) ([]*plumbing.Reference, error) {
				if strings.Contains(sourceURL, "cnb.cool") {
					return nil, errTestResolve
				}
				return targetBranchReferences(target), nil
			},
			cloneErr: errTestClone,
			wantCode: protocol.CodeGitCloneFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := mustParseTarget(t, "v5.4.0")
			client := &fakeGitClient{
				list: func(_ context.Context, sourceURL string, _ []byte) ([]*plumbing.Reference, error) {
					return tt.list(target, sourceURL)
				},
				clone: func(context.Context, string, git.CloneOptions) error {
					return tt.cloneErr
				},
			}
			fetcher := mustTestFetcher(t, successfulFetcherDependencies(t, client))
			_, err := fetcher.Fetch(t.Context(), FetchRequest{
				Plan:        mustGitPlan(t, "cnb"),
				Target:      target,
				OperationID: "OPERATION-3",
			})
			assertGitrepoCode(t, err, tt.wantCode)
		})
	}
}

func TestFetcher_CancelCleansTemporaryRepository(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	plan := mustGitPlan(t, "cnb")
	layout := mustGitLayout(t)
	cloneEntered := make(chan struct{})
	client := &fakeGitClient{
		list: func(_ context.Context, _ string, _ []byte) ([]*plumbing.Reference, error) {
			return targetBranchReferences(target), nil
		},
		clone: func(ctx context.Context, _ string, _ git.CloneOptions) error {
			close(cloneEntered)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	type cleanupObservation struct {
		request filesystem.DeleteRequest
		ctxErr  error
	}
	cleanupCalled := make(chan cleanupObservation, 1)
	remover := &fakeTreeRemover{
		remove: func(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			cleanupCalled <- cleanupObservation{request: request, ctxErr: ctx.Err()}
			return filesystem.DeleteResult{Removed: true, AuditCompleted: true}, nil
		},
	}
	deps := successfulFetcherDependencies(t, client)
	deps.layout = layout
	deps.remover = remover
	fetcher := mustTestFetcher(t, deps)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	type fetchOutcome struct {
		result FetchResult
		err    error
	}
	done := make(chan fetchOutcome, 1)
	var stages []protocol.Stage
	go func() {
		result, err := fetcher.Fetch(ctx, FetchRequest{
			Plan:        plan,
			Target:      target,
			OperationID: "OPERATION-CANCEL",
			StageReporter: func(stage protocol.Stage) {
				stages = append(stages, stage)
			},
		})
		done <- fetchOutcome{result: result, err: err}
	}()

	<-cloneEntered
	cancel()
	outcome := <-done
	if outcome.result != (FetchResult{}) {
		t.Fatalf("Fetch() result = %#v, want zero on cancellation", outcome.result)
	}
	assertGitrepoCode(t, outcome.err, protocol.CodeOperationCancelled)
	observation := <-cleanupCalled
	if observation.ctxErr != nil {
		t.Fatalf("cleanup context error = %v, want independent live context", observation.ctxErr)
	}
	request := observation.request
	wantPath, err := layout.RepoUpdateDir("OPERATION-CANCEL")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if request.Kind != filesystem.DeleteRepositoryUpdate || request.Target != wantPath || request.OperationID != "OPERATION-CANCEL" {
		t.Fatalf("cleanup request = %#v, want exact repository update", request)
	}
	if len(stages) == 0 || stages[len(stages)-1] != protocol.StageWorkspaceCleanup {
		t.Fatalf("reported stages = %v, want cleanup last", stages)
	}
}

func TestFetcher_ExistingTemporaryRepositoryIsPreserved(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	layout := mustGitLayout(t)
	updatePath, err := layout.RepoUpdateDir("OPERATION-EXISTING")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if err := os.MkdirAll(updatePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	marker := filepath.Join(updatePath, "owner-marker")
	if err := os.WriteFile(marker, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	clientCalled := false
	client := &fakeGitClient{
		list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
			clientCalled = true
			return targetBranchReferences(target), nil
		},
		clone: func(context.Context, string, git.CloneOptions) error {
			clientCalled = true
			return git.ErrRepositoryAlreadyExists
		},
	}
	deps := successfulFetcherDependencies(t, client)
	deps.layout = layout
	deps.remover = &fakeTreeRemover{
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called for pre-existing update directory")
			return filesystem.DeleteResult{}, nil
		},
	}
	fetcher := mustTestFetcher(t, deps)

	_, err = fetcher.Fetch(t.Context(), FetchRequest{
		Plan:        mustGitPlan(t, "cnb"),
		Target:      target,
		OperationID: "OPERATION-EXISTING",
	})
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	if clientCalled {
		t.Fatal("Git client called despite pre-existing update directory")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "foreign" {
		t.Fatalf("marker after Fetch() = %q, %v; want preserved", got, readErr)
	}
}

func TestCloneProgressWriter_DoesNotExposeOrParseGitText(t *testing.T) {
	var events []protocol.ProgressEvent
	writer := newCloneProgressWriter(func(event protocol.ProgressEvent) error {
		events = append(events, event)
		return nil
	})
	secret := []byte("remote: https://user:token@example.test/repo.git fatal: 99%\n")

	for i := 0; i < maxCloneProgressPulses+10; i++ {
		written, err := writer.Write(secret)
		if err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if written != len(secret) {
			t.Fatalf("Write() = %d, want %d", written, len(secret))
		}
	}
	if len(events) != maxCloneProgressPulses {
		t.Fatalf("event count = %d, want bounded %d", len(events), maxCloneProgressPulses)
	}
	for _, event := range events {
		if event.Stage != protocol.StageWorkspaceClone || event.Status != protocol.ProgressRunning {
			t.Fatalf("progress event = %#v, want workspace.clone/running", event)
		}
		if event.Message != cloneProgressPulseMessage {
			t.Fatalf("progress message = %q, want fixed pulse", event.Message)
		}
		if strings.Contains(event.Message, "token") || strings.Contains(event.Message, "99%") || event.Percent != nil {
			t.Fatalf("progress event exposes or interprets Git text: %#v", event)
		}
	}
}

type fakeGitClient struct {
	list  func(ctx context.Context, sourceURL string, caBundle []byte) ([]*plumbing.Reference, error)
	clone func(ctx context.Context, path string, options git.CloneOptions) error
}

func (f *fakeGitClient) ListReferences(ctx context.Context, sourceURL string, caBundle []byte) ([]*plumbing.Reference, error) {
	return f.list(ctx, sourceURL, caBundle)
}

func (f *fakeGitClient) Clone(ctx context.Context, path string, options git.CloneOptions) error {
	return f.clone(ctx, path, options)
}

type fakeCloneVerifier struct {
	verify func(ctx context.Context, request VerificationRequest) (Revision, error)
}

func (f *fakeCloneVerifier) Verify(ctx context.Context, request VerificationRequest) (Revision, error) {
	return f.verify(ctx, request)
}

type fakeTreeRemover struct {
	remove func(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error)
}

func (f *fakeTreeRemover) RemoveTree(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
	return f.remove(ctx, request)
}

type progressRecorder struct {
	mu     sync.Mutex
	events []protocol.ProgressEvent
}

func (r *progressRecorder) EmitProgress(event protocol.ProgressEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *progressRecorder) Events() []protocol.ProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.ProgressEvent(nil), r.events...)
}

func mustParseTarget(t *testing.T, version string) Target {
	t.Helper()
	target, err := ParseTarget(version)
	if err != nil {
		t.Fatalf("ParseTarget(%q) error = %v", version, err)
	}
	return target
}

func mustGitLayout(t *testing.T) *config.Layout {
	t.Helper()
	base := t.TempDir()
	layout, err := config.NewLayout(filepath.Join(base, "app"), base)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return layout
}

func mustGitPlan(t *testing.T, preferred string) mirror.Plan {
	t.Helper()
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{
		Preferred: map[mirror.Kind]string{mirror.KindGit: preferred},
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := mirror.BuildPlan(catalog, policy, mirror.KindGit)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	return plan
}

func mustTestRotator(t *testing.T) *mirror.Rotator {
	t.Helper()
	rotator, err := mirror.NewRotator(
		mirror.WithMaxSourceAttempts(1),
		mirror.WithRotatorWait(func(context.Context, time.Duration) error { return nil }),
	)
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	return rotator
}

func successfulFetcherDependencies(t *testing.T, client gitClient) fetcherDependencies {
	t.Helper()
	return fetcherDependencies{
		layout:  mustGitLayout(t),
		rotator: mustTestRotator(t),
		git:     client,
		remover: &fakeTreeRemover{
			remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
				return filesystem.DeleteResult{Removed: true, AuditCompleted: true}, nil
			},
		},
		verifier: &fakeCloneVerifier{
			verify: func(_ context.Context, request VerificationRequest) (Revision, error) {
				return newRevision(request.Target, testGitCommit, request.Source)
			},
		},
		emitProgress: (&progressRecorder{}).EmitProgress,
	}
}

func mustTestFetcher(t *testing.T, dependencies fetcherDependencies) *Fetcher {
	t.Helper()
	fetcher, err := newFetcherWithDependencies(dependencies)
	if err != nil {
		t.Fatalf("newFetcherWithDependencies() error = %v", err)
	}
	return fetcher
}

func targetBranchReferences(target Target) []*plumbing.Reference {
	return []*plumbing.Reference{
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(target.Branch()), plumbing.NewHash(testGitCommit)),
	}
}

func assertGitrepoCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if got := operationErr.Code(); got != want {
		t.Fatalf("error code = %q, want %q (error = %v)", got, want, err)
	}
}
