package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestProductionLogger_WritesFragmentsWithoutRepeatingEvent(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	var stderr bytes.Buffer
	runtimeLogger, err := logging.New(
		t.Context(),
		layout,
		&stderr,
		"backend supervise",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",
		logging.WithClock(func() time.Time { return time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC) }),
	)
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	logger := productionLogger{logger: runtimeLogger}
	for _, record := range []process.StreamRecord{
		{Stream: process.StreamStdout, Fragment: "fragment-one", LineID: 1},
		{Stream: process.StreamStdout, Fragment: "fragment-two", Event: "fragment-onefragment-two", LineID: 1, EndOfLine: true},
	} {
		if err := logger.Record(t.Context(), record); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}
	path := logger.LogPath()
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, fragment := range []string{"fragment-one", "fragment-two"} {
		if got := bytes.Count(payload, []byte(`"message":"`+fragment+`"`)); got != 1 {
			t.Fatalf("log fragment %q count = %d, want 1; payload=%s", fragment, got, payload)
		}
	}
	if bytes.Contains(payload, []byte(`"message":"fragment-onefragment-two"`)) {
		t.Fatalf("runtime log repeated the protocol event: %s", payload)
	}
}

func TestBackendManaged_PreconditionsFailClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*backendFixture)
		want   protocol.Code
	}{
		{name: "missing entry", mutate: func(f *backendFixture) { f.entry.err = errors.New("missing") }, want: protocol.CodeBackendEntryNotFound},
		{name: "environment not ready", mutate: func(f *backendFixture) { f.state.environment.Status = protocol.StateEnvironmentBroken }, want: protocol.CodeEnvironmentBroken},
		{name: "revision mismatch", mutate: func(f *backendFixture) { f.repository.Commit = stringsRepeat("b", 40) }, want: protocol.CodeEnvironmentBroken},
	}

	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			f := newBackendFixture(t)
			test.mutate(f)
			err := f.supervisor().Supervise(t.Context(), f.request())
			assertBackendCode(t, err, test.want)
			if f.uv.startCalls != 0 {
				t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
			}
			if got := f.emitter.states(); len(got) != 0 {
				t.Fatalf("state events = %#v, want none", got)
			}
		})
	}
}

func TestBackendManaged_UsesExactUVArgsAndEnvironment(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.checkErr = nil
	f.proc.keepAlive = true

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()

	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}

	if got, want := f.uv.args, []string{"run", "--project", f.layout.RepoDir(), "--no-sync", "main.py"}; !equalStrings(got, want) {
		t.Fatalf("uv args = %#v, want %#v", got, want)
	}
	if f.uv.options.Identity == nil {
		t.Fatal("managed identity is nil")
	}
	if f.uv.options.Stage != protocol.StageBackendSpawn || f.uv.options.ProjectDir != f.layout.RepoDir() {
		t.Fatalf("managed run options = %#v, want backend.spawn and managed repo", f.uv.options.RunOptions)
	}
	if len(f.uv.options.Environment) != 0 {
		t.Fatalf("backend injected raw environment = %#v, want typed supervision only", f.uv.options.Environment)
	}
	if got := *f.uv.options.Identity; got.Version != f.state.environment.LastSuccessful.Version || got.Commit != f.state.environment.LastSuccessful.Commit {
		t.Fatalf("managed identity = %#v, want protocol/version/commit", got)
	}
	if !f.proc.terminated || !f.proc.waitedEmpty || !f.proc.closed {
		t.Fatalf("process cleanup = terminated:%v waitEmpty:%v closed:%v", f.proc.terminated, f.proc.waitedEmpty, f.proc.closed)
	}
}

func TestBackendManaged_SpawnFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.startErr = errors.New("spawn failed")

	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendSpawnFailed)
	if got := f.emitter.states(); len(got) != 0 {
		t.Fatalf("state events = %#v, want none before process creation", got)
	}
	if f.logger.closeCalls != 1 {
		t.Fatalf("logger close calls = %d, want 1", f.logger.closeCalls)
	}
}

func TestBackendManaged_ReadyEmitsRunningState(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)

	states := f.emitter.states()
	if len(states) != 2 || states[0].Status != protocol.StateStartingBackend || states[1].Status != protocol.StateRunning {
		t.Fatalf("state events = %#v, want starting_backend then running", states)
	}
	if got := states[1].Details["pid"]; got != f.proc.pid {
		t.Fatalf("running pid detail = %#v, want %d", got, f.proc.pid)
	}
	if got := states[1].Details["baseUrl"]; got != "http://127.0.0.1:36163" {
		t.Fatalf("running baseUrl = %#v", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
}

func TestBackendManaged_PreReadyExitFlushesLogsBeforeErrorResult(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.exitImmediately = true
	f.health.observeExit = true

	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendExitedBeforeReady)
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr.Details()["exitCode"] != 0 {
		t.Fatalf("error details = %#v, want exitCode=0", backendErr)
	}
	if len(f.logger.records) == 0 {
		t.Fatal("logger records are empty, want stream flush")
	}
	if f.logger.closeCalls != 1 {
		t.Fatalf("logger close calls = %d, want 1", f.logger.closeCalls)
	}
	if got := f.emitter.states(); len(got) < 2 || got[len(got)-1].Status != protocol.StateBackendFailed {
		t.Fatalf("state events = %#v, want backend_failed cleanup state", got)
	}
}

func TestBackendSuperviseContract(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}

	for _, event := range f.emitter.eventsSnapshot() {
		if event == "error" || event == "result" {
			t.Fatalf("backend emitted forbidden %s event", event)
		}
	}
}

func TestBackendManaged_StartupLogsFlushAfterStarting(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.startRecords = []process.StreamRecord{{Stream: process.StreamStdout, Fragment: "fragment", Event: "boot", EndOfLine: true}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	events := f.emitter.eventsSnapshot()
	starting, log := indexOfEvent(events, "state:starting_backend"), indexOfEvent(events, "log:boot")
	if starting < 0 || log < 0 || starting >= log {
		t.Fatalf("events = %#v, want starting_backend before boot log", events)
	}
	if len(f.logger.records) != 1 || f.logger.records[0].Fragment != "fragment" {
		t.Fatalf("logger records = %#v, want original Fragment", f.logger.records)
	}
}

func TestBackendManaged_LiveTransactionPIDIsInconsistent(t *testing.T) {
	f := newBackendFixture(t)
	f.state.transaction = &Transaction{PID: 7331, Handle: &fakeTransaction{}}
	f.pid = &fakePID{alive: true}
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeUpdateStateAmbiguous)
	var coded *Error
	if !errors.As(err, &coded) || coded.Details()["reason"] != "transaction_pid_alive_without_backend_mutex" {
		t.Fatalf("error = %#v, want inconsistent transaction details", err)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackendManaged_LockConflictPreservesCodeOnlyError(t *testing.T) {
	f := newBackendFixture(t)
	f.lock.acquireErr = &fakeCodeError{code: protocol.CodeBackendAlreadyRunning}
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendAlreadyRunning)
	if f.lock.closeCalls != 1 || f.state.closeCalls != 1 {
		t.Fatalf("close calls lock/state = %d/%d, want 1/1", f.lock.closeCalls, f.state.closeCalls)
	}
}

func TestBackendManaged_LockErrorClosesReturnedLease(t *testing.T) {
	f := newBackendFixture(t)
	lease := &fakeLease{}
	f.lock.acquireLease = lease
	f.lock.acquireErr = errors.New("acquire returned an owned lease")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeMutexOperationFailed)
	if !lease.closed {
		t.Fatal("lease returned with an error was not closed")
	}
}

func TestBackendManaged_UVCheckPreservesTypedCode(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.checkErr = &fakeCodeError{code: protocol.CodeUVVersionMismatch}
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeUVVersionMismatch)
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackendManaged_EnvironmentReadFailureIncludesReason(t *testing.T) {
	f := newBackendFixture(t)
	f.state.environmentErr = errors.New("environment file is missing")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeEnvironmentBroken)
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr.Details()["field"] != "environment" || backendErr.Details()["reason"] != "read_failed" {
		t.Fatalf("error details = %#v, want environment/read_failed", backendErr)
	}
}

func TestBackendManaged_BrokenEnvironmentKeepsFailureFacts(t *testing.T) {
	f := newBackendFixture(t)
	f.state.environment.Status = protocol.StateEnvironmentBroken
	f.state.environment.Broken = &state.BrokenEnvironment{
		Reason:   state.ReasonOperationFailed,
		Stage:    protocol.StageDependenciesSync,
		ExitCode: 50,
		LogPath:  f.logger.path,
	}
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeEnvironmentBroken)
	var backendErr *Error
	if !errors.As(err, &backendErr) ||
		backendErr.Details()["reason"] != string(state.ReasonOperationFailed) ||
		backendErr.Details()["brokenStage"] != string(protocol.StageDependenciesSync) ||
		backendErr.Details()["exitCode"] != 50 ||
		backendErr.Details()["logPath"] == "" {
		t.Fatalf("error details = %#v, want persisted broken facts", backendErr)
	}
}

func TestBackendManaged_FinalRevisionReadFailureKeepsReason(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.errOnCall = 2
	f.repository.err = errors.New("repository identity read failed")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeEnvironmentBroken)
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr.Details()["reason"] != "read_failed" {
		t.Fatalf("error details = %#v, want reason=read_failed", backendErr)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackendManaged_FinalEnvironmentReadKeepsBrokenFacts(t *testing.T) {
	f := newBackendFixture(t)
	broken := f.state.environment
	broken.Status = protocol.StateEnvironmentBroken
	broken.Broken = &state.BrokenEnvironment{
		Reason:   state.ReasonRepositoryChanged,
		Stage:    protocol.StageWorkspaceSwap,
		ExitCode: 0,
		LogPath:  f.logger.path,
	}
	f.state.secondEnvironment = &broken
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeEnvironmentBroken)
	var backendErr *Error
	if !errors.As(err, &backendErr) ||
		backendErr.Details()["reason"] != string(state.ReasonRepositoryChanged) ||
		backendErr.Details()["brokenStage"] != string(protocol.StageWorkspaceSwap) ||
		backendErr.Details()["logPath"] != f.logger.path {
		t.Fatalf("error details = %#v, want final broken environment facts", backendErr)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackendManaged_LocalCancellationWinsPreflightAndSpawn(t *testing.T) {
	t.Run("environment read", func(t *testing.T) {
		f := newBackendFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		f.state.onReadEnvironment = cancel
		f.state.environmentErr = context.Canceled
		err := f.supervisor().Supervise(ctx, f.request())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Supervise() error = %v, want context.Canceled", err)
		}
	})

	t.Run("process start", func(t *testing.T) {
		f := newBackendFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		f.uv.onStart = cancel
		f.uv.startErr = context.Canceled
		err := f.supervisor().Supervise(ctx, f.request())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Supervise() error = %v, want context.Canceled", err)
		}
	})
}

func TestBackendManaged_CleanupFailureOverridesCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.waitEmptyErr = errors.New("job membership cannot be confirmed")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	err := <-done
	assertBackendCode(t, err, protocol.CodeBackendShutdownFailed)
}

func TestBackendManaged_CancellationDoesNotBecomeShutdownFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.waitErr = context.Canceled
	f.proc.sinkOnTerminate = true
	f.logger.respectContext = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	var coded interface{ Code() protocol.Code }
	if errors.As(err, &coded) && coded.Code() == protocol.CodeBackendShutdownFailed {
		t.Fatalf("cancellation was misclassified as %s: %v", coded.Code(), err)
	}
}

func TestBackendManaged_CancellationKeepsSiblingWaitFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.waitErr = errors.Join(context.Canceled, errors.New("wait failed"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	assertBackendCode(t, <-done, protocol.CodeBackendShutdownFailed)
}

func TestBackendManaged_CleanupFailureOverridesHealthFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.health.err = newError(protocol.CodeBackendHealthInvalid, protocol.StageBackendHealth, "后端健康响应无效", nil, errors.New("invalid health"))
	f.proc.waitEmptyErr = errors.New("job membership cannot be confirmed")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendShutdownFailed)
	var backendErr *Error
	if !errors.As(err, &backendErr) ||
		backendErr.Details()["operation"] != "wait_empty" ||
		backendErr.Details()["logPath"] != f.logger.path ||
		backendErr.Details()["pid"] != f.proc.pid ||
		backendErr.Details()["exitCode"] != 0 {
		t.Fatalf("cleanup details = %#v, want operation/logPath/pid/exitCode", backendErr)
	}
}

func TestBackendManaged_HealthUpdateFailureCleansAndEmitsFailed(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.state.updateErrStage = protocol.StageBackendHealth
	f.state.updateErr = errors.New("state update failed")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeStateWriteFailed)
	states := f.emitter.states()
	if len(states) < 2 || states[len(states)-1].Status != protocol.StateBackendFailed {
		t.Fatalf("states = %#v, want backend_failed after starting", states)
	}
	if !f.proc.terminated || !f.proc.waitedEmpty || !f.proc.closed || f.logger.closeCalls != 1 {
		t.Fatalf("cleanup process/logger = %v/%v/%v/%d", f.proc.terminated, f.proc.waitedEmpty, f.proc.closed, f.logger.closeCalls)
	}
}

func TestBackendManaged_NonNilResourcesWithErrorsAreClosed(t *testing.T) {
	f := newBackendFixture(t)
	f.loggerErr = errors.New("logger open failed")
	err := f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeInternalError)
	if f.logger.closeCalls != 1 {
		t.Fatalf("logger close calls = %d, want 1", f.logger.closeCalls)
	}

	f = newBackendFixture(t)
	f.state.beginHandle = &fakeTransaction{}
	f.state.beginErr = errors.New("transaction write outcome is uncertain")
	err = f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeStateWriteFailed)
	if f.state.removeCalls != 1 || f.logger.closeCalls != 1 {
		t.Fatalf("transaction/logger cleanup calls = %d/%d, want 1/1", f.state.removeCalls, f.logger.closeCalls)
	}

	f = newBackendFixture(t)
	f.uv.startErr = errors.New("spawn failed after process creation")
	f.uv.returnProcOnErr = true
	err = f.supervisor().Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendSpawnFailed)
	if !f.proc.terminated || !f.proc.waitedEmpty || !f.proc.closed || f.logger.closeCalls != 1 {
		t.Fatalf("non-nil process cleanup = %v/%v/%v logger=%d", f.proc.terminated, f.proc.waitedEmpty, f.proc.closed, f.logger.closeCalls)
	}
}

func indexOfEvent(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

type backendFixture struct {
	t               *testing.T
	layout          *config.Layout
	emitter         *fakeEmitter
	lock            *fakeLock
	state           *fakeState
	repository      *fakeRepository
	entry           *fakeEntry
	uv              *fakeUV
	health          *fakeHealth
	logger          *fakeLogger
	loggerErr       error
	proc            *fakeProcess
	pid             *fakePID
	depsHTTP        HTTPCloser
	shutdownTimeout time.Duration
}

func newBackendFixture(t *testing.T) *backendFixture {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	f := &backendFixture{
		t:          t,
		layout:     layout,
		emitter:    &fakeEmitter{running: make(chan struct{})},
		lock:       &fakeLock{},
		state:      &fakeState{environment: state.EnvironmentState{Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: "v1.2.3", Commit: stringsRepeat("a", 40)}}},
		repository: &fakeRepository{Healthy: true, Version: "v1.2.3", Commit: stringsRepeat("a", 40)},
		entry:      &fakeEntry{},
		uv:         &fakeUV{},
		health:     &fakeHealth{},
		logger:     &fakeLogger{path: filepath.Join(root, "backend.log")},
		proc:       &fakeProcess{pid: 4242},
	}
	f.uv.proc = f.proc
	return f
}

func (f *backendFixture) supervisor() *ManagedSupervisor {
	f.t.Helper()
	s, err := NewManagedSupervisor(f.layout, Dependencies{
		Lock:       f.lock,
		State:      f.state,
		Repository: f.repository,
		Entry:      f.entry,
		UV:         f.uv,
		Health:     f.health,
		Logger:     func(context.Context, Request) (Logger, error) { return f.logger, f.loggerErr },
		Clock:      func() time.Time { return time.Unix(1, 0).UTC() },
		UVPath:     "uv.exe",
		PythonPath: "python.exe",
		PID:        f.pid,
		NewTimer:   func(time.Duration) Timer { return immediateTimer{} },
	})
	if err != nil {
		f.t.Fatalf("NewManagedSupervisor() error = %v", err)
	}
	return s
}

func (f *backendFixture) request() Request {
	return Request{OperationID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", RuntimePID: 9001, Emitter: f.emitter}
}

func assertBackendCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) {
		t.Fatalf("error = %v, want coded error %s", err, want)
	}
	if got := coded.Code(); got != want {
		t.Fatalf("error code = %s, want %s", got, want)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func stringsRepeat(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type fakeEmitter struct {
	mu             sync.Mutex
	events         []string
	state          []protocol.StateEvent
	stateErr       error
	logErr         error
	running        chan struct{}
	runningOnce    sync.Once
	runningRelease <-chan struct{}
}

func (e *fakeEmitter) EmitState(event protocol.StateEvent) error {
	var release <-chan struct{}
	e.mu.Lock()
	e.events = append(e.events, "state:"+string(event.Status))
	e.state = append(e.state, event)
	if event.Status == protocol.StateRunning && e.running != nil {
		e.runningOnce.Do(func() { close(e.running) })
	}
	if event.Status == protocol.StateRunning {
		release = e.runningRelease
	}
	e.mu.Unlock()
	if release != nil {
		<-release
	}
	e.mu.Lock()
	err := e.stateErr
	e.mu.Unlock()
	return err
}

func (e *fakeEmitter) EmitLog(event protocol.LogEvent) error {
	e.mu.Lock()
	e.events = append(e.events, "log:"+event.Message)
	err := e.logErr
	e.mu.Unlock()
	return err
}

func (e *fakeEmitter) EmitWarning(event protocol.WarningEvent) error {
	e.mu.Lock()
	e.events = append(e.events, "warning:"+event.Code)
	e.mu.Unlock()
	return nil
}

func (e *fakeEmitter) states() []protocol.StateEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.StateEvent(nil), e.state...)
}

func (e *fakeEmitter) eventsSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

type fakeLock struct {
	acquireErr     error
	acquireLease   Lease
	acquireStarted chan struct{}
	acquireBlock   chan struct{}
	closeCalls     int
	closeErr       error
}

func (l *fakeLock) Acquire(ctx context.Context) (Lease, error) {
	if l.acquireStarted != nil {
		select {
		case <-l.acquireStarted:
		default:
			close(l.acquireStarted)
		}
	}
	if l.acquireBlock != nil {
		select {
		case <-l.acquireBlock:
		case <-ctx.Done():
			return l.acquireLease, ctx.Err()
		}
	}
	if l.acquireErr != nil {
		return l.acquireLease, l.acquireErr
	}
	return &fakeLease{}, nil
}
func (l *fakeLock) Close() error { l.closeCalls++; return l.closeErr }

type fakeLease struct {
	closed   bool
	closeErr error
}

func (l *fakeLease) Close() error { l.closed = true; return l.closeErr }

type fakeState struct {
	environment            state.EnvironmentState
	environmentErr         error
	onReadEnvironment      func()
	secondEnvironment      *state.EnvironmentState
	secondEnvironmentAfter int
	readCalls              int
	closeCalls             int
	closeErr               error
	transaction            *Transaction
	updateErr              error
	updateErrStage         protocol.Stage
	updateStarted          chan struct{}
	updateBlock            chan struct{}
	updateCalls            int
	beginHandle            TransactionHandle
	beginErr               error
	beginStarted           chan struct{}
	beginBlock             chan struct{}
	removeCalls            int
}

func (s *fakeState) ReadEnvironment(context.Context) (state.EnvironmentState, error) {
	s.readCalls++
	if s.onReadEnvironment != nil {
		s.onReadEnvironment()
	}
	threshold := s.secondEnvironmentAfter
	if threshold <= 0 {
		threshold = 2
	}
	if s.secondEnvironment != nil && s.readCalls >= threshold {
		return *s.secondEnvironment, s.environmentErr
	}
	return s.environment, s.environmentErr
}

func (s *fakeState) ReadBackendTransaction(context.Context) (Transaction, error) {
	if s.transaction != nil {
		return *s.transaction, nil
	}
	return Transaction{}, ErrTransactionNotFound
}

func (s *fakeState) BeginBackendTransaction(ctx context.Context, _ TransactionInput) (TransactionHandle, error) {
	if s.beginStarted != nil {
		select {
		case <-s.beginStarted:
		default:
			close(s.beginStarted)
		}
	}
	if s.beginBlock != nil {
		select {
		case <-s.beginBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.beginHandle != nil || s.beginErr != nil {
		return s.beginHandle, s.beginErr
	}
	return &fakeTransaction{}, nil
}

func (s *fakeState) UpdateBackendTransaction(_ context.Context, _ TransactionHandle, stage protocol.Stage) error {
	s.updateCalls++
	if s.updateStarted != nil && stage == s.updateErrStage {
		select {
		case <-s.updateStarted:
		default:
			close(s.updateStarted)
		}
	}
	if s.updateBlock != nil && stage == s.updateErrStage {
		<-s.updateBlock
	}
	if s.updateErrStage != "" && s.updateErrStage == stage {
		return s.updateErr
	}
	return nil
}

func (s *fakeState) RemoveBackendTransaction(context.Context, TransactionHandle) error {
	s.removeCalls++
	return nil
}
func (s *fakeState) Close() error {
	s.closeCalls++
	return s.closeErr
}

type fakeRepository struct {
	Healthy      bool
	Version      string
	Commit       string
	errOnCall    int
	err          error
	checks       int
	started      chan struct{}
	block        chan struct{}
	blockOnCall  int
	blockStarted chan struct{}
}

func (r *fakeRepository) Check(ctx context.Context) (RepositoryResult, error) {
	r.checks++
	if r.started != nil {
		select {
		case <-r.started:
		default:
			close(r.started)
		}
	}
	if r.block != nil && (r.blockOnCall == 0 || r.checks == r.blockOnCall) {
		if r.blockStarted != nil {
			select {
			case <-r.blockStarted:
			default:
				close(r.blockStarted)
			}
		}
		select {
		case <-r.block:
		case <-ctx.Done():
			return RepositoryResult{}, ctx.Err()
		}
	}
	if r.errOnCall != 0 && r.checks == r.errOnCall {
		return RepositoryResult{}, r.err
	}
	return RepositoryResult{Healthy: r.Healthy, Version: r.Version, Commit: r.Commit}, nil
}

type fakeEntry struct{ err error }

func (e *fakeEntry) Check(context.Context, string) error { return e.err }

type fakeUV struct {
	proc            ManagedProcess
	procSequence    []ManagedProcess
	args            []string
	options         uv.ManagedOptions
	startErr        error
	checkErr        error
	startCalls      int
	returnProcOnErr bool
	onStart         func()
}

func (u *fakeUV) Check(context.Context) error { return u.checkErr }
func (u *fakeUV) Executable() string          { return "uv.exe" }
func (u *fakeUV) StartManaged(_ context.Context, args []string, options uv.ManagedOptions, sink process.StreamSink) (ManagedProcess, error) {
	u.startCalls++
	u.args = append([]string(nil), args...)
	u.options = options
	if u.onStart != nil {
		u.onStart()
	}
	proc := u.proc
	if index := u.startCalls - 1; index >= 0 && index < len(u.procSequence) {
		proc = u.procSequence[index]
	}
	if u.startErr != nil {
		if u.returnProcOnErr {
			if process, ok := proc.(*fakeProcess); ok {
				process.SetSink(sink)
			}
			return proc, u.startErr
		}
		return nil, u.startErr
	}
	if process, ok := proc.(*fakeProcess); ok {
		process.SetSink(sink)
	}
	return proc, nil
}

type fakeHealth struct {
	err         error
	observeExit bool
	started     chan struct{}
	block       <-chan struct{}
}

func (h *fakeHealth) Check(ctx context.Context, _ health.Expectation, probe health.Probe) error {
	if h.started != nil {
		select {
		case <-h.started:
		default:
			close(h.started)
		}
	}
	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if h.observeExit {
		select {
		case <-probe.Exited():
			return newError(protocol.CodeBackendExitedBeforeReady, protocol.StageBackendHealth, "后端在就绪前退出", nil, nil)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.err
}

type fakeLogger struct {
	mu             sync.Mutex
	path           string
	records        []process.StreamRecord
	closeCalls     int
	respectContext bool
}

func (l *fakeLogger) Record(ctx context.Context, record process.StreamRecord) error {
	if l.respectContext {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.records = append(l.records, record)
	l.mu.Unlock()
	return nil
}
func (l *fakeLogger) LogPath() string { return l.path }
func (l *fakeLogger) Close() error {
	l.mu.Lock()
	l.closeCalls++
	l.mu.Unlock()
	return nil
}

type fakeTransaction struct{}

type fakeProcess struct {
	mu              sync.Mutex
	pid             uint32
	keepAlive       bool
	exitImmediately bool
	exited          chan struct{}
	terminated      bool
	waitedEmpty     bool
	closed          bool
	sink            process.StreamSink
	startRecords    []process.StreamRecord
	sinkOnTerminate bool
	waitErr         error
	waitErrs        []error
	waitEmptyErr    error
	waitEmptyErrs   []error
	cleanupStarted  chan struct{}
	cleanupRelease  chan struct{}
	cleanupOnce     sync.Once
	terminateErr    error
	closeErr        error
}

func (p *fakeProcess) SetSink(sink process.StreamSink) {
	p.mu.Lock()
	p.sink = sink
	if p.exited == nil {
		p.exited = make(chan struct{})
	}
	if p.exitImmediately {
		close(p.exited)
		if sink != nil {
			_ = sink(context.Background(), process.StreamRecord{Stream: process.StreamStdout, Event: "ready", EndOfLine: true})
		}
	}
	for _, record := range p.startRecords {
		if sink != nil {
			_ = sink(context.Background(), record)
		}
	}
	p.mu.Unlock()
}

func (p *fakeProcess) EmitRecord(ctx context.Context, record process.StreamRecord) error {
	p.mu.Lock()
	sink := p.sink
	p.mu.Unlock()
	if sink == nil {
		return errors.New("fake process sink is nil")
	}
	return sink(ctx, record)
}

func (p *fakeProcess) PID() uint32 { return p.pid }
func (p *fakeProcess) Exited() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited == nil {
		p.exited = make(chan struct{})
	}
	return p.exited
}
func (p *fakeProcess) Snapshot() ([]process.Info, error) {
	return []process.Info{{PID: p.pid, Executable: "uv.exe"}, {PID: p.pid + 1, ParentPID: p.pid, Executable: "python.exe"}}, nil
}
func (p *fakeProcess) Wait(context.Context) (process.ExitResult, error) {
	if len(p.waitErrs) > 0 {
		err := p.waitErrs[0]
		p.waitErrs = p.waitErrs[1:]
		return process.ExitResult{ExitCode: 0}, err
	}
	return process.ExitResult{ExitCode: 0}, p.waitErr
}
func (p *fakeProcess) Terminate(uint32) error {
	p.mu.Lock()
	p.terminated = true
	if p.sinkOnTerminate && p.sink != nil {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_ = p.sink(cancelled, process.StreamRecord{Stream: process.StreamStderr, Fragment: "tail", Event: "tail", EndOfLine: true})
	}
	if p.exited != nil {
		select {
		case <-p.exited:
		default:
			close(p.exited)
		}
	}
	p.mu.Unlock()
	return p.terminateErr
}

func (p *fakeProcess) Exit() {
	p.mu.Lock()
	if p.exited == nil {
		p.exited = make(chan struct{})
	}
	select {
	case <-p.exited:
	default:
		close(p.exited)
	}
	p.mu.Unlock()
}
func (p *fakeProcess) WaitEmpty(context.Context) error {
	if p.cleanupStarted != nil {
		p.cleanupOnce.Do(func() { close(p.cleanupStarted) })
	}
	if p.cleanupRelease != nil {
		<-p.cleanupRelease
	}
	p.waitedEmpty = true
	if len(p.waitEmptyErrs) > 0 {
		err := p.waitEmptyErrs[0]
		p.waitEmptyErrs = p.waitEmptyErrs[1:]
		return err
	}
	return p.waitEmptyErr
}
func (p *fakeProcess) Close() error {
	p.closed = true
	return p.closeErr
}

type fakePID struct{ alive bool }

func (p *fakePID) Alive(context.Context, uint32) (bool, error) { return p.alive, nil }

type fakeCodeError struct{ code protocol.Code }

type immediateTimer struct{}

func (immediateTimer) C() <-chan time.Time {
	channel := make(chan time.Time)
	close(channel)
	return channel
}

func (immediateTimer) Stop() bool { return true }

func (e *fakeCodeError) Error() string       { return string(e.code) }
func (e *fakeCodeError) Code() protocol.Code { return e.code }
