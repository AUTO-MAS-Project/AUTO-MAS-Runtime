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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestService_CheckIsReadOnly(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	reader := &serviceTestReader{snapshot: testServiceSnapshot("v1.0.0")}
	calledLocks := false
	calledRuntime := false
	service, err := newServiceWithDependencies(
		layout,
		reader,
		func(context.Context, *config.Layout) (mutationLockSet, error) {
			calledLocks = true
			return nil, nil
		},
		func(context.Context, *config.Layout, SyncRequest, OperationLogger) (syncRuntime, error) {
			calledRuntime = true
			return nil, nil
		},
		func(mirror.Policy) (mirror.Plan, error) { return mirror.Plan{}, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}
	before := directoryNames(t, root)
	result, err := service.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.Healthy || result.Reason != "ok" {
		t.Fatalf("Check() = %#v, want healthy ok", result)
	}
	if calledLocks || calledRuntime {
		t.Fatalf("Check() touched mutation runtime: locks=%t runtime=%t", calledLocks, calledRuntime)
	}
	if after := directoryNames(t, root); strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("Check() changed app-root entries: before=%v after=%v", before, after)
	}
}

func TestService_CheckRejectsReparseAncestor(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "container")
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatalf("MkdirAll(container) error = %v", err)
	}
	externalApp := filepath.Join(external, "app")
	if err := os.MkdirAll(externalApp, 0o755); err != nil {
		t.Fatalf("MkdirAll(external app) error = %v", err)
	}
	alias := filepath.Join(container, "alias")
	mustCreateGitRepoJunction(t, alias, external)
	layout, err := config.NewLayout(filepath.Join(alias, "app"), container)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	reader := &serviceTestReader{snapshot: testServiceSnapshot("v1.0.0")}
	service, err := newServiceWithDependencies(
		layout,
		reader,
		func(context.Context, *config.Layout) (mutationLockSet, error) {
			t.Fatal("Check() acquired mutation locks through a reparse ancestor")
			return nil, nil
		},
		func(context.Context, *config.Layout, SyncRequest, OperationLogger) (syncRuntime, error) {
			t.Fatal("Check() built runtime through a reparse ancestor")
			return nil, nil
		},
		func(mirror.Policy) (mirror.Plan, error) { return mirror.Plan{}, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}
	result, err := service.Check(t.Context())
	if err != nil {
		t.Fatalf("Check() error = %v, want completed invalid result", err)
	}
	if result.Healthy || result.Reason != "invalid" {
		t.Fatalf("Check() = %#v, want unhealthy invalid result", result)
	}
}

func TestService_SyncActualSwapInvalidatesReadyEnvironment(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	reader := &serviceTestReader{err: errors.New("invalid active repository")}
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StateReadyToStart,
		LastSuccessful: state.Revision{Version: "v0.9.0", Commit: strings.Repeat("b", 40)},
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "c")}
	runtime.swapResult = SwapResult{Revision: runtime.fetchResult.Revision, RepositoryActivated: true, MutationApplied: true, CleanupCompleted: true}
	service := newTestService(t, layout, reader, runtime, nil)
	result, err := service.Sync(context.Background(), testSyncRequest(target))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Changed || result.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("Sync() = %#v, want changed environment_broken", result)
	}
	if runtime.writtenEnvironment.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("written environment status = %q, want environment_broken", runtime.writtenEnvironment.Status)
	}
	if runtime.writtenEnvironment.Broken == nil || runtime.writtenEnvironment.Broken.Reason != state.ReasonRepositoryChanged {
		t.Fatalf("written environment broken = %#v, want repository_changed", runtime.writtenEnvironment.Broken)
	}
	if runtime.writtenEnvironment.LastSuccessful.Version != "v0.9.0" {
		t.Fatalf("lastSuccessful version = %q, want v0.9.0", runtime.writtenEnvironment.LastSuccessful.Version)
	}
}

func TestService_SyncNoOpPreservesStableState(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	reader := &serviceTestReader{snapshot: testServiceSnapshot(target.Version())}
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StateReadyToStart,
		LastSuccessful: state.Revision{Version: target.Version(), Commit: strings.Repeat("d", 40)},
	}
	service := newTestService(t, layout, reader, runtime, nil)
	result, err := service.Sync(context.Background(), testSyncRequest(target))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Changed || result.Status != protocol.StateReadyToStart {
		t.Fatalf("Sync() = %#v, want unchanged ready_to_start", result)
	}
	if runtime.fetchCalls != 0 || runtime.swapCalls != 0 || runtime.writeEnvironmentCalls != 0 {
		t.Fatalf("no-op touched mutation: fetch=%d swap=%d environmentWrites=%d", runtime.fetchCalls, runtime.swapCalls, runtime.writeEnvironmentCalls)
	}
}

func TestService_SyncRejectsRunningBackend(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	locks := &serviceTestLocks{acquireErr: serviceCodedError{code: protocol.CodeBackendStillRunning}}
	service, err := newServiceWithDependencies(
		layout,
		&serviceTestReader{},
		func(context.Context, *config.Layout) (mutationLockSet, error) { return locks, nil },
		func(context.Context, *config.Layout, SyncRequest, OperationLogger) (syncRuntime, error) {
			return nil, errors.New("runtime must not be built")
		},
		func(mirror.Policy) (mirror.Plan, error) { return mirror.Plan{}, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	_, err = service.Sync(context.Background(), testSyncRequest(target))
	if err == nil {
		t.Fatal("Sync() error = nil, want backend conflict")
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Code() != protocol.CodeBackendStillRunning {
		t.Fatalf("Sync() error = %v, want BACKEND_STILL_RUNNING", err)
	}
}

func TestService_StateWriteFailureDoesNotReportSuccess(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StateReadyToStart,
		LastSuccessful: state.Revision{Version: "v0.9.0", Commit: strings.Repeat("e", 40)},
	}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "f")}
	runtime.swapResult = SwapResult{Revision: runtime.fetchResult.Revision, RepositoryActivated: true, MutationApplied: true, CleanupCompleted: true}
	runtime.writeEnvironmentErr = errors.New("state write failed")
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)
	_, err := service.Sync(context.Background(), testSyncRequest(target))
	if err == nil {
		t.Fatal("Sync() error = nil, want state write failure")
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Code() != protocol.CodeStateWriteFailed {
		t.Fatalf("Sync() error = %v, want STATE_WRITE_FAILED", err)
	}
	if runtime.updateRemovals != 0 {
		t.Fatalf("update transaction removals = %d, want retained transaction", runtime.updateRemovals)
	}
}

func TestService_ActiveCleanupDeadlineIsStateFailure(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StateReadyToStart,
		LastSuccessful: state.Revision{Version: "v0.9.0", Commit: strings.Repeat("e", 40)},
	}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "a")}
	runtime.swapResult = SwapResult{
		Revision:            runtime.fetchResult.Revision,
		MutationApplied:     true,
		RepositoryActivated: true,
	}
	runtime.writeTransactionErrByStage = map[protocol.Stage]error{
		protocol.StageWorkspaceCleanup: context.DeadlineExceeded,
	}
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

	_, err := service.Sync(t.Context(), testSyncRequest(target))
	assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync() error = %v, want retained deadline cause", err)
	}
}

func TestService_PartialSwapRecoversBeforeStableState(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateReadyToStart}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "8")}
	ctx, cancel := context.WithCancel(t.Context())
	runtime.swap = func(context.Context, SwapRequest) (SwapResult, error) {
		cancel()
		return SwapResult{Revision: runtime.fetchResult.Revision, MutationApplied: true}, newError(
			protocol.CodeGitRepoSwapFailed,
			protocol.StageWorkspaceSwap,
			messageForCode(protocol.CodeGitRepoSwapFailed),
			map[string]any{},
			errors.New("second rename failed"),
		)
	}
	runtime.recover = func(call int, recoverCtx context.Context, _ RecoveryRequest) (RecoveryResult, error) {
		if call == 2 && recoverCtx.Err() != nil {
			t.Fatalf("partial-swap recovery context = %v, want independent live context", recoverCtx.Err())
		}
		return RecoveryResult{
			Recovered:          call == 2,
			MutationApplied:    call == 2,
			TransactionRemoved: call == 2,
		}, nil
	}
	emitter := &recordingServiceEmitter{}
	request := testSyncRequest(target)
	request.Emitter = emitter
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

	_, err := service.Sync(ctx, request)
	assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
	if runtime.recoverCalls != 2 {
		t.Fatalf("Recover() calls = %d, want initial plus partial-swap recovery", runtime.recoverCalls)
	}
	if !emitter.hasState(protocol.StateReadyToStart) {
		t.Fatalf("state events = %v, want ready_to_start after recovery", emitter.statuses())
	}
}

func TestService_PartialSwapRecoveryFailureDoesNotEmitStableState(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateReadyToStart}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "9")}
	runtime.swapResult = SwapResult{Revision: runtime.fetchResult.Revision, MutationApplied: true}
	runtime.swapErr = newError(protocol.CodeGitRepoSwapFailed, protocol.StageWorkspaceSwap, messageForCode(protocol.CodeGitRepoSwapFailed), map[string]any{}, errors.New("second rename failed"))
	runtime.recover = func(call int, _ context.Context, _ RecoveryRequest) (RecoveryResult, error) {
		if call == 2 {
			return RecoveryResult{}, newError(protocol.CodeUpdateStateAmbiguous, protocol.StageWorkspaceCleanup, messageForCode(protocol.CodeUpdateStateAmbiguous), map[string]any{"reason": "rollback_failed"}, errors.New("rollback failed"))
		}
		return RecoveryResult{}, nil
	}
	emitter := &recordingServiceEmitter{}
	request := testSyncRequest(target)
	request.Emitter = emitter
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

	_, err := service.Sync(t.Context(), request)
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	if runtime.recoverCalls != 2 {
		t.Fatalf("Recover() calls = %d, want 2", runtime.recoverCalls)
	}
	if emitter.hasState(protocol.StateReadyToStart) {
		t.Fatalf("state events = %v, must not claim ready_to_start", emitter.statuses())
	}
}

func TestService_StateWriteFailureOutranksPriorCleanupFailure(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateReadyToStart}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "7")}
	runtime.swapResult = SwapResult{Revision: runtime.fetchResult.Revision, MutationApplied: true, RepositoryActivated: true}
	runtime.swapErr = newError(protocol.CodeGitRepoCleanupFailed, protocol.StageWorkspaceCleanup, messageForCode(protocol.CodeGitRepoCleanupFailed), map[string]any{}, errors.New("retired cleanup failed"))
	runtime.writeEnvironmentErr = errors.New("state write failed")
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

	_, err := service.Sync(t.Context(), testSyncRequest(target))
	assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
	if !strings.Contains(err.Error(), "retired cleanup failed") {
		t.Fatalf("Sync() error = %v, want retained cleanup cause", err)
	}
}

func TestService_ActivatedRepositoryIgnoresLateCancellationDuringCommit(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	target := mustServiceTarget(t, "v1.0.0")
	runtime := newServiceTestRuntime()
	runtime.environment = state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateReadyToStart}
	runtime.fetchResult = FetchResult{Revision: testServiceRevision(target, "6")}
	ctx, cancel := context.WithCancel(t.Context())
	runtime.swap = func(context.Context, SwapRequest) (SwapResult, error) {
		cancel()
		return SwapResult{
			Revision:            runtime.fetchResult.Revision,
			MutationApplied:     true,
			RepositoryActivated: true,
			CleanupCompleted:    true,
		}, nil
	}
	runtime.writeEnvironment = func(commitCtx context.Context, value state.EnvironmentState) error {
		if err := commitCtx.Err(); err != nil {
			return err
		}
		runtime.writtenEnvironment = value
		return nil
	}
	service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

	result, err := service.Sync(ctx, testSyncRequest(target))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Changed || result.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("Sync() = %#v, want committed environment_broken", result)
	}
	if runtime.writtenEnvironment.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("written environment = %#v, want environment_broken", runtime.writtenEnvironment)
	}
}

func TestService_EarlyMutationTransactionFailureRollsBackStableState(t *testing.T) {
	tests := []struct {
		name                string
		initial             protocol.StateStatus
		newTransactionErr   error
		writeTransactionErr error
	}{
		{
			name:              "ready new transaction failure",
			initial:           protocol.StateReadyToStart,
			newTransactionErr: errors.New("mutation transaction construction failed"),
		},
		{
			name:                "ready write transaction failure",
			initial:             protocol.StateReadyToStart,
			writeTransactionErr: errors.New("mutation transaction write failed"),
		},
		{
			name:              "broken new transaction failure",
			initial:           protocol.StateEnvironmentBroken,
			newTransactionErr: errors.New("mutation transaction construction failed"),
		},
		{
			name:                "broken write transaction failure",
			initial:             protocol.StateEnvironmentBroken,
			writeTransactionErr: errors.New("mutation transaction write failed"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout := testServiceLayout(t, root)
			if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
				t.Fatalf("Mkdir(repo) error = %v", err)
			}
			target := mustServiceTarget(t, "v1.0.0")
			runtime := newServiceTestRuntime()
			runtime.environment = state.EnvironmentState{
				SchemaVersion: state.SchemaVersion,
				Status:        test.initial,
				LastSuccessful: state.Revision{
					Version: "v0.9.0",
					Commit:  strings.Repeat("e", 40),
				},
			}
			runtime.newTransactionErr = test.newTransactionErr
			runtime.writeTransactionErr = test.writeTransactionErr
			emitter := &recordingServiceEmitter{}
			request := testSyncRequest(target)
			request.Emitter = emitter
			service := newTestService(t, layout, &serviceTestReader{err: errors.New("invalid active repository")}, runtime, nil)

			_, err := service.Sync(t.Context(), request)
			assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
			if !emitter.hasState(protocol.StateSyncingRepository) {
				t.Fatalf("state events = %v, want syncing_repository before failure", emitter.statuses())
			}
			if !emitter.hasState(test.initial) {
				t.Fatalf("state events = %v, want rollback to %q", emitter.statuses(), test.initial)
			}
			if runtime.fetchCalls != 0 || runtime.swapCalls != 0 {
				t.Fatalf("early transaction failure touched repository mutation: fetch=%d swap=%d", runtime.fetchCalls, runtime.swapCalls)
			}
			if got := emitter.states[len(emitter.states)-1].Status; got != test.initial {
				t.Fatalf("last state event = %q, want rollback status %q", got, test.initial)
			}
			if got := emitter.states[len(emitter.states)-1].Stage; got != protocol.StageWorkspaceCleanup {
				t.Fatalf("rollback state stage = %q, want %q", got, protocol.StageWorkspaceCleanup)
			}
			if got := emitter.states[len(emitter.states)-1].Message; got != "仓库同步未改变当前工作区" {
				t.Fatalf("rollback state message = %q, want stable rollback message", got)
			}
			if runtime.newTransactionCalls != 1 {
				t.Fatalf("NewTransaction() calls = %d, want 1", runtime.newTransactionCalls)
			}
			if test.newTransactionErr == nil && runtime.writeTransactionCalls != 1 {
				t.Fatalf("WriteTransaction() calls = %d, want 1", runtime.writeTransactionCalls)
			}
			if test.newTransactionErr != nil && runtime.writeTransactionCalls != 0 {
				t.Fatalf("WriteTransaction() calls = %d, want 0 after construction failure", runtime.writeTransactionCalls)
			}
			if runtime.environment.Status != test.initial {
				t.Fatalf("persisted environment status = %q, want unchanged %q", runtime.environment.Status, test.initial)
			}
			if runtime.writeEnvironmentCalls != 0 {
				t.Fatalf("WriteEnvironment() calls = %d, want 0", runtime.writeEnvironmentCalls)
			}
		})
	}
}

func TestServiceCleanupContext_IgnoresCancellationAfterCreation(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	cleanup, cancelCleanup := serviceCleanupContext(parent)
	defer cancelCleanup()
	cancelParent()
	if err := cleanup.Err(); err != nil {
		t.Fatalf("cleanup context error = %v, want independent live context", err)
	}
}

type serviceTestReader struct {
	snapshot repositorySnapshot
	err      error
}

func (r *serviceTestReader) Inspect(context.Context, string) (repositorySnapshot, error) {
	if r.err != nil {
		return repositorySnapshot{}, r.err
	}
	return r.snapshot, nil
}

type serviceTestLocks struct {
	acquireErr error
	lease      *serviceTestLease
	closed     bool
}

func (l *serviceTestLocks) AcquireMutation(context.Context) (mutationLease, error) {
	if l.acquireErr != nil {
		return nil, l.acquireErr
	}
	if l.lease == nil {
		l.lease = &serviceTestLease{}
	}
	return l.lease, nil
}
func (l *serviceTestLocks) Close() error { l.closed = true; return nil }

type serviceTestLease struct{ closed bool }

func (l *serviceTestLease) Close() error { l.closed = true; return nil }

type serviceCodedError struct{ code protocol.Code }

func (e serviceCodedError) Error() string       { return string(e.code) }
func (e serviceCodedError) Code() protocol.Code { return e.code }

type serviceTestRuntime struct {
	mu                         sync.Mutex
	environment                state.EnvironmentState
	readEnvironmentErr         error
	writeEnvironmentErr        error
	writeEnvironment           func(context.Context, state.EnvironmentState) error
	writtenEnvironment         state.EnvironmentState
	fetchResult                FetchResult
	swapResult                 SwapResult
	swapErr                    error
	swap                       func(context.Context, SwapRequest) (SwapResult, error)
	recover                    func(int, context.Context, RecoveryRequest) (RecoveryResult, error)
	newTransactionErr          error
	writeTransactionErr        error
	writeTransactionErrByStage map[protocol.Stage]error
	recoverCalls               int
	fetchCalls                 int
	swapCalls                  int
	newTransactionCalls        int
	writeTransactionCalls      int
	writeEnvironmentCalls      int
	updateRemovals             int
}

func newServiceTestRuntime() *serviceTestRuntime { return &serviceTestRuntime{} }

func (r *serviceTestRuntime) Recover(ctx context.Context, request RecoveryRequest) (RecoveryResult, error) {
	r.recoverCalls++
	if r.recover != nil {
		return r.recover(r.recoverCalls, ctx, request)
	}
	return RecoveryResult{}, nil
}
func (r *serviceTestRuntime) ReadEnvironment(context.Context) (state.EnvironmentState, error) {
	return r.environment, r.readEnvironmentErr
}
func (r *serviceTestRuntime) NewTransaction(_ state.TransactionKind, input state.TransactionInput) (state.TransactionState, error) {
	r.newTransactionCalls++
	if r.newTransactionErr != nil {
		return state.TransactionState{}, r.newTransactionErr
	}
	return state.TransactionState{SchemaVersion: state.SchemaVersion, OperationID: input.OperationID, Command: input.Command, PID: input.PID, StartedAt: time.Now().UTC(), TargetVersion: input.TargetVersion, Stage: input.Stage}, nil
}
func (r *serviceTestRuntime) WriteTransaction(_ context.Context, _ state.TransactionKind, value state.TransactionState) error {
	r.writeTransactionCalls++
	if err := r.writeTransactionErrByStage[value.Stage]; err != nil {
		return err
	}
	return r.writeTransactionErr
}
func (r *serviceTestRuntime) ReadTransaction(context.Context, state.TransactionKind) (state.TransactionSnapshot, error) {
	return state.TransactionSnapshot{}, nil
}
func (r *serviceTestRuntime) RemoveTransaction(_ context.Context, snapshot state.TransactionSnapshot) error {
	r.updateRemovals++
	return nil
}
func (r *serviceTestRuntime) Fetch(context.Context, FetchRequest) (FetchResult, error) {
	r.fetchCalls++
	return r.fetchResult, nil
}
func (r *serviceTestRuntime) Swap(ctx context.Context, request SwapRequest) (SwapResult, error) {
	r.swapCalls++
	if r.swap != nil {
		return r.swap(ctx, request)
	}
	return r.swapResult, r.swapErr
}
func (r *serviceTestRuntime) NewBrokenEnvironment(last state.Revision, broken state.BrokenEnvironment) (state.EnvironmentState, error) {
	return state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateEnvironmentBroken, LastSuccessful: last, Broken: &broken}, nil
}
func (r *serviceTestRuntime) WriteEnvironment(ctx context.Context, value state.EnvironmentState) error {
	r.writeEnvironmentCalls++
	if r.writeEnvironment != nil {
		return r.writeEnvironment(ctx, value)
	}
	r.writtenEnvironment = value
	return r.writeEnvironmentErr
}
func (r *serviceTestRuntime) Close() error { return nil }

type serviceTestLogger struct{}

func (serviceTestLogger) LogPath() string { return filepath.Join("C:\\runtime", "workspace-sync.log") }
func (serviceTestLogger) Close() error    { return nil }

type serviceTestAuditor struct{}

func (serviceTestAuditor) RecordDeletion(context.Context, filesystem.DeleteAuditRecord) error {
	return nil
}

type serviceTestEmitter struct{}

func (serviceTestEmitter) EmitProgress(protocol.ProgressEvent) error { return nil }
func (serviceTestEmitter) EmitState(protocol.StateEvent) error       { return nil }

type recordingServiceEmitter struct {
	states []protocol.StateEvent
}

func (*recordingServiceEmitter) EmitProgress(protocol.ProgressEvent) error { return nil }

func (e *recordingServiceEmitter) EmitState(event protocol.StateEvent) error {
	e.states = append(e.states, event)
	return nil
}

func (e *recordingServiceEmitter) hasState(status protocol.StateStatus) bool {
	for _, event := range e.states {
		if event.Status == status {
			return true
		}
	}
	return false
}

func (e *recordingServiceEmitter) statuses() []protocol.StateStatus {
	statuses := make([]protocol.StateStatus, 0, len(e.states))
	for _, event := range e.states {
		statuses = append(statuses, event.Status)
	}
	return statuses
}

func newTestService(t *testing.T, layout *config.Layout, reader repositoryReader, runtime *serviceTestRuntime, lockSet mutationLockSet) *Service {
	t.Helper()
	if lockSet == nil {
		lockSet = &serviceTestLocks{}
	}
	service, err := newServiceWithDependencies(
		layout,
		reader,
		func(context.Context, *config.Layout) (mutationLockSet, error) { return lockSet, nil },
		func(context.Context, *config.Layout, SyncRequest, OperationLogger) (syncRuntime, error) {
			return runtime, nil
		},
		func(mirror.Policy) (mirror.Plan, error) { return mirror.Plan{}, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}
	return service
}

func testSyncRequest(target Target) SyncRequest {
	return SyncRequest{
		Target:      target,
		OperationID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		PID:         1234,
		Emitter:     serviceTestEmitter{},
		LoggerFactory: func(context.Context, string, string) (OperationLogger, error) {
			return serviceTestLogger{}, nil
		},
		Auditor: serviceTestAuditor{},
		Clock:   time.Now,
	}
}

func testServiceLayout(t *testing.T, root string) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return layout
}

func mustServiceTarget(t *testing.T, version string) Target {
	t.Helper()
	target, err := ParseTarget(version)
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	return target
}

func testServiceRevision(target Target, prefix string) Revision {
	return Revision{version: target.Version(), branch: target.Branch(), commit: strings.Repeat(prefix, 40), sourceKey: "github"}
}

func testServiceSnapshot(version string) repositorySnapshot {
	catalog, _ := mirror.DefaultCatalog()
	source, _ := catalog.Source(mirror.KindGit, "github")
	commit := strings.Repeat("a", 40)
	target, _ := ParseTarget(version)
	return repositorySnapshot{
		nonBare:        true,
		remotes:        []remoteSnapshot{{name: "origin", fetchURLs: []string{source.BaseURL()}}},
		headSymbolic:   true,
		headTarget:     plumbing.NewBranchReferenceName(target.Branch()).String(),
		commit:         commit,
		shallow:        []string{commit},
		versionMode:    filemode.Regular,
		versionPayload: []byte(`{"version":"` + version + `"}`),
	}
}

func directoryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}
