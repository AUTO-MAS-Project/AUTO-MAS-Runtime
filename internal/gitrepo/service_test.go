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
	mu                    sync.Mutex
	environment           state.EnvironmentState
	readEnvironmentErr    error
	writeEnvironmentErr   error
	writtenEnvironment    state.EnvironmentState
	fetchResult           FetchResult
	swapResult            SwapResult
	fetchCalls            int
	swapCalls             int
	writeEnvironmentCalls int
	updateRemovals        int
}

func newServiceTestRuntime() *serviceTestRuntime { return &serviceTestRuntime{} }
func (r *serviceTestRuntime) Recover(context.Context, RecoveryRequest) (RecoveryResult, error) {
	return RecoveryResult{}, nil
}
func (r *serviceTestRuntime) ReadEnvironment(context.Context) (state.EnvironmentState, error) {
	return r.environment, r.readEnvironmentErr
}
func (r *serviceTestRuntime) NewTransaction(_ state.TransactionKind, input state.TransactionInput) (state.TransactionState, error) {
	return state.TransactionState{SchemaVersion: state.SchemaVersion, OperationID: input.OperationID, Command: input.Command, PID: input.PID, StartedAt: time.Now().UTC(), TargetVersion: input.TargetVersion, Stage: input.Stage}, nil
}
func (r *serviceTestRuntime) WriteTransaction(context.Context, state.TransactionKind, state.TransactionState) error {
	return nil
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
func (r *serviceTestRuntime) Swap(context.Context, SwapRequest) (SwapResult, error) {
	r.swapCalls++
	return r.swapResult, nil
}
func (r *serviceTestRuntime) NewBrokenEnvironment(last state.Revision, broken state.BrokenEnvironment) (state.EnvironmentState, error) {
	return state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateEnvironmentBroken, LastSuccessful: last, Broken: &broken}, nil
}
func (r *serviceTestRuntime) WriteEnvironment(_ context.Context, value state.EnvironmentState) error {
	r.writeEnvironmentCalls++
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
