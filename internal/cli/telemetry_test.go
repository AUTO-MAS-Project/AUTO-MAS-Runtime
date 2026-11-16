package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

type telemetryRecorderSpy struct {
	mu            sync.Mutex
	internal      []telemetry.InternalObservation
	closeCalls    int
	beforeResult  bool
	resultWritten *atomic.Bool
	closeWaits    bool
}

func (r *telemetryRecorderSpy) CaptureInternal(_ context.Context, observation telemetry.InternalObservation) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultWritten != nil && !r.resultWritten.Load() {
		r.beforeResult = true
	}
	r.internal = append(r.internal, observation)
	return "0123456789abcdef0123456789abcdef"
}

func (r *telemetryRecorderSpy) Close(ctx context.Context) {
	r.mu.Lock()
	r.closeCalls++
	waits := r.closeWaits
	r.mu.Unlock()
	if waits {
		<-ctx.Done()
	}
}

func (r *telemetryRecorderSpy) snapshot() (internal []telemetry.InternalObservation, closeCalls int, beforeResult bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.InternalObservation(nil), r.internal...), r.closeCalls, r.beforeResult
}

type resultTrackingWriter struct {
	bytes.Buffer
	resultWritten *atomic.Bool
}

type resultFailingWriter struct {
	bytes.Buffer
}

type resultPanickingWriter struct {
	bytes.Buffer
}

func (w *resultPanickingWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte(`"type":"result"`)) {
		panic("secret result writer panic")
	}
	return w.Buffer.Write(value)
}

func (w *resultFailingWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte(`"type":"result"`)) {
		return 0, errors.New("injected result write failure")
	}
	return w.Buffer.Write(value)
}

func (w *resultTrackingWriter) Write(value []byte) (int, error) {
	n, err := w.Buffer.Write(value)
	if w.resultWritten != nil && bytes.Contains(value, []byte(`"type":"result"`)) {
		w.resultWritten.Store(true)
	}
	return n, err
}

func executeWithTelemetry(t *testing.T, args []string, recorder telemetry.Recorder, output io.Writer, additional ...Option) int {
	t.Helper()
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	options := []Option{
		WithCWD(t.TempDir()),
		WithClock(func() time.Time { return time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC) }),
		WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }),
	}
	options = append(options, additional...)
	var stderr bytes.Buffer
	return Execute(context.Background(), args, IO{In: strings.NewReader(""), Out: output, Err: &stderr}, options...)
}

func TestRunOperation_TelemetryAfterFinalResult(t *testing.T) {
	var resultWritten atomic.Bool
	recorder := &telemetryRecorderSpy{resultWritten: &resultWritten}
	var stdout resultTrackingWriter
	stdout.resultWritten = &resultWritten
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "version"}, recorder, &stdout,
		WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{Version: "v1.2.3", Protocol: protocol.Version}, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	internal, closes, beforeResult := recorder.snapshot()
	if len(internal) != 0 || closes != 1 {
		t.Fatalf("telemetry counts = internal:%d close:%d, want 0/1", len(internal), closes)
	}
	if beforeResult {
		t.Fatal("telemetry was recorded before the final result")
	}
	events := parseNDJSON(t, stdout.String())
	if got := eventType(events[len(events)-1]); got != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", got)
	}
}

func TestRunOperation_TelemetryNeverWritesProtocolOutput(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "version"}, recorder, &stdout)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	events := parseNDJSON(t, stdout.String())
	if got := eventType(events[len(events)-1]); got != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", got)
	}
}

func TestRunOperation_OfflineHasZeroTelemetryNetworkCalls(t *testing.T) {
	var factoryCalls atomic.Int32
	var stdout bytes.Buffer
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	code := Execute(context.Background(), []string{"--output", "ndjson", "--offline", "version"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: io.Discard},
		WithCWD(t.TempDir()),
		WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { factoryCalls.Add(1); return &telemetryRecorderSpy{} }),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("telemetry factory calls = %d, want 0 in offline mode", got)
	}
}

func TestRunOperation_InternalErrorReportsSentryOnly(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, errors.New("internal test failure")
			}}, nil
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	internal, _, _ := recorder.snapshot()
	if len(internal) != 1 || internal[0].Code != string(protocol.CodeInternalError) {
		t.Fatalf("internal observations = %#v, want one INTERNAL_ERROR", internal)
	}
}

func TestRunOperation_ExpectedFailureReportsSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, doctor.NewError(protocol.CodeNetworkUnavailable, protocol.StageDoctor, "网络不可用", map[string]any{"reason": "probe_failed"}, errors.New("expected failure"))
			}}, nil
		}),
	)
	if code != protocol.ExitCodeNetworkFailure {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeNetworkFailure)
	}
	internal, _, _ := recorder.snapshot()
	if len(internal) != 1 || internal[0].Code != string(protocol.CodeNetworkUnavailable) || internal[0].Reason != "probe_failed" {
		t.Fatalf("internal observations = %#v, want one sanitized expected failure", internal)
	}
}

func TestTelemetryReason_RejectsUnboundedDetails(t *testing.T) {
	tests := []struct {
		name    string
		details map[string]any
		want    string
	}{
		{name: "safe", details: map[string]any{"reason": "repository_unknown"}, want: "repository_unknown"},
		{name: "path", details: map[string]any{"reason": `C:\\Users\\secret`}},
		{name: "free text", details: map[string]any{"reason": "repository unknown"}},
		{name: "non string", details: map[string]any{"reason": 42}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := telemetryReason(test.details); got != test.want {
				t.Fatalf("telemetryReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunOperation_ExpectedFailureReportsSanitizedDiagnostics(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	secret := `Authorization: Basic super-secret C:\Users\alice\runtime\update.json`
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, doctor.NewError(protocol.CodeUpdateStateAmbiguous, protocol.StageDoctor, "现场异常", map[string]any{
					"path": secret,
					"attempts": []map[string]any{{
						"source": "official", "outcome": "failed", "failureKind": "repository_invalid",
					}},
					"password": "do-not-upload",
				}, errors.New(secret))
			}}, nil
		}),
	)
	if code != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %#v, want one", observations)
	}
	if len(observations[0].DiagnosticDetails) == 0 {
		t.Fatalf("diagnostics = %#v, want useful structured payload", observations[0].DiagnosticDetails)
	}
	encoded, err := json.Marshal(observations[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"alice", "super-secret", "do-not-upload", "Authorization", `C:\\Users`} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("sanitized observation contains %q: %s", forbidden, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte("repository_invalid")) {
		t.Fatalf("sanitized observation = %s, want stable failureKind", encoded)
	}
}

func TestBootstrap_InvalidPersistedStatusReportsReason(t *testing.T) {
	root := t.TempDir()
	recorder := &telemetryRecorderSpy{}
	log := &m5TestLog{}
	payload, err := json.Marshal(state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StatePreparingUV,
		UpdatedAt:      time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		LastSuccessful: state.Revision{Version: "v5.4.0", Commit: strings.Repeat("a", 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	var statePath string
	preparationStarted := false
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0"}, recorder, &stdout,
		WithCWD(root),
		WithEnvironmentStateStoreFactory(func(ctx context.Context, layout *config.Layout, clock func() time.Time) (environmentStateStore, error) {
			statePath = layout.EnvironmentStateFile()
			if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(statePath, payload, 0o644); err != nil {
				return nil, err
			}
			return state.NewStore(ctx, layout, state.WithClock(clock))
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			preparationStarted = true
			return &m5TestEnvironment{calls: &log.calls}, nil
		}),
	)
	if code != protocol.ExitCodeOperationConflict || preparationStarted {
		t.Fatalf("exit/preparation = %d/%t, want %d/false", code, preparationStarted, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].Code != "STATE_WRITE_FAILED" || observations[0].Reason != "invalid_lifecycle_state" {
		t.Fatalf("observations = %#v, want existing code with invalid lifecycle reason", observations)
	}
	remaining, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(remaining, payload) {
		t.Fatalf("state changed after rejection: payload=%q, error=%v", remaining, err)
	}
}

type telemetryDiagnosticError struct {
	error
	details map[string]any
}

func (e telemetryDiagnosticError) Unwrap() error                     { return e.error }
func (e telemetryDiagnosticError) DiagnosticDetails() map[string]any { return e.details }

func TestRunOperation_PreservesNestedFailureDiagnostics(t *testing.T) {
	legacy := map[string]any{"failureKind": "dependency_sync_nonzero", "exitCode": 0}
	primary := doctor.NewError(protocol.CodeDependencySyncFailed, protocol.StageDependenciesSync, "同步失败", legacy, errors.New("private cause"))
	inner := telemetryDiagnosticError{error: primary, details: map[string]any{
		"failureKind": "output_read_failed", "version": "v5.8.0", "password": "private-secret",
		"mutex":       map[string]any{"operation": "create-mutex", "component": "mutation", "windowsError": 5},
		"directories": []map[string]any{{"component": "repo", "state": "missing"}},
	}}
	failure := telemetryDiagnosticError{error: errors.Join(inner), details: map[string]any{"version": "v5.8.1"}}
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	exitCode := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, failure
			}}, nil
		}),
	)
	if exitCode != protocol.ExitCodeEnvironmentFailure {
		t.Fatalf("exit code = %d, want %d", exitCode, protocol.ExitCodeEnvironmentFailure)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %#v, want one", observations)
	}
	details := observations[0].DiagnosticDetails
	if details["failureKind"] != "output_read_failed" || details["version"] != "v5.8.1" || details["exitCode"] != 0 || details["mutex"] == nil || details["directories"] == nil || details["password"] != nil {
		t.Fatalf("diagnostics = %#v, want nested facts with outer precedence and sanitization", details)
	}
	events := parseNDJSON(t, stdout.String())
	if got := events[len(events)-1].object["details"]; !reflect.DeepEqual(got, map[string]any{"failureKind": "dependency_sync_nonzero", "exitCode": float64(0)}) {
		t.Fatalf("protocol details = %#v, want original facts", got)
	}
	if !reflect.DeepEqual(legacy, map[string]any{"failureKind": "dependency_sync_nonzero", "exitCode": 0}) {
		t.Fatalf("source details mutated: %#v", legacy)
	}
}

func TestBootstrap_MutexFailureReportsDiagnostics(t *testing.T) {
	failure := &lock.OperationError{Operation: "create-mutex", Kind: lock.KindMutation, Name: "private-mutex-name", Cause: syscall.Errno(5)}
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	exitCode := executeWithTelemetry(t, []string{"--output", "ndjson", "bootstrap", "--version", "v5.8.0"}, recorder, &stdout,
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return nil, failure
		}),
	)
	if exitCode != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", exitCode, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].Code != string(protocol.CodeMutexOperationFailed) {
		t.Fatalf("observations = %#v, want mutex failure", observations)
	}
	want := map[string]any{"mutex": map[string]any{"component": "mutation", "operation": "create-mutex", "windowsError": uint64(5)}}
	want["fileSystem"] = expectedSessionFileSystem(t, observations)
	if got := observations[0].DiagnosticDetails; !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
	if strings.Contains(stdout.String(), `"mutex"`) {
		t.Fatalf("protocol gained telemetry-only facts: %s", stdout.String())
	}
}

type telemetryExecutionError struct {
	*commandError
	diagnostics map[string]any
}

func (e telemetryExecutionError) DiagnosticDetails() map[string]any { return e.diagnostics }

func TestRunOperation_DiagnosticsRespectSelectedFailure(t *testing.T) {
	cleanup := &commandError{code: protocol.CodeStateWriteFailed, stage: protocol.StageWorkspaceCleanup, message: "清理失败", details: map[string]any{"exitCode": 42}}
	execution := telemetryExecutionError{
		commandError: &commandError{code: protocol.CodeDependencySyncFailed, stage: protocol.StageDependenciesSync, message: "执行失败"},
		diagnostics:  map[string]any{"failureKind": "output_read_failed", "exitCode": 0},
	}
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	exitCode := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, errors.Join(cleanup, execution)
			}}, nil
		}),
	)
	if exitCode != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", exitCode, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].Code != string(protocol.CodeStateWriteFailed) {
		t.Fatalf("observations = %#v, want selected cleanup failure", observations)
	}
	if got := observations[0].DiagnosticDetails; !reflect.DeepEqual(got, map[string]any{"exitCode": 42, "fileSystem": expectedSessionFileSystem(t, observations)}) {
		t.Fatalf("diagnostics = %#v, want selected failure facts", got)
	}
}

func TestBackendTelemetry_PreservesPrimaryAndCleanupFacts(t *testing.T) {
	primary := &commandError{code: protocol.CodeBackendHealthTimeout, stage: protocol.StageBackendHealth, message: "timeout", committed: true}
	cleanup := &commandError{code: protocol.CodeStateWriteFailed, stage: protocol.StageBackendCleanup, message: "cleanup", committed: true, details: map[string]any{"exitCode": 1}}
	err := telemetryDiagnosticError{error: errors.Join(cleanup, primary), details: map[string]any{"mode": "managed", "version": "v1.2.3", "commit": strings.Repeat("a", 40), "terminationRequested": true, "password": "secret"}}
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	exitCode := executeWithTelemetry(t, []string{"--output", "ndjson", "backend", "supervise", "--mode", "managed"}, recorder, &stdout,
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(context.Context, backend.Request) error { return err }), nil
		}),
	)
	if exitCode != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", exitCode, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].Code != string(protocol.CodeStateWriteFailed) {
		t.Fatalf("observations = %#v, want existing cleanup priority", observations)
	}
	want := map[string]any{
		"mode": "managed", "version": "v1.2.3", "commit": strings.Repeat("a", 40),
		"fileSystem": expectedSessionFileSystem(t, observations),
		"exitCode":   1, "terminationRequested": true,
		"failures": []any{
			map[string]any{"phase": "primary", "code": "BACKEND_HEALTH_TIMEOUT", "stage": "backend.health"},
			map[string]any{"phase": "cleanup", "code": "STATE_WRITE_FAILED", "stage": "backend.cleanup"},
		},
	}
	if !reflect.DeepEqual(observations[0].DiagnosticDetails, want) {
		t.Fatalf("diagnostics = %#v, want %#v", observations[0].DiagnosticDetails, want)
	}
	if !reflect.DeepEqual(cleanup.details, map[string]any{"exitCode": 1}) {
		t.Fatalf("original details mutated: %#v", cleanup.details)
	}
	events := parseNDJSON(t, stdout.String())
	result := events[len(events)-1]
	if details, ok := result.object["details"].(map[string]any); !ok || details["version"] != nil || details["failures"] != nil || details["terminationRequested"] != nil {
		t.Fatalf("protocol result gained telemetry-only fields: %#v", result.object["details"])
	}
	if bytes.Contains(stdout.Bytes(), []byte("terminationRequested")) {
		t.Fatalf("NDJSON gained telemetry-only termination fact: %s", stdout.Bytes())
	}
}

func TestRunOperation_UnreportableFailureDoesNotReportSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, doctor.NewError(protocol.CodeInvalidArgument, protocol.StageDoctor, "参数无效", map[string]any{}, errors.New("bad input"))
			}}, nil
		}),
	)
	if code != protocol.ExitCodeInvalidArgument {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeInvalidArgument)
	}
	observations, closes, _ := recorder.snapshot()
	if len(observations) != 0 || closes != 1 {
		t.Fatalf("telemetry = observations:%d close:%d, want 0/1", len(observations), closes)
	}
}

func TestRunOperation_ExpectedFailureNeverUploadsRawError(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	secret := `api_key=super-secret D:\private\repo`
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, doctor.NewError(protocol.CodeUpdateStateAmbiguous, protocol.StageDoctor, "现场异常", map[string]any{"failureKind": "repository_unknown"}, errors.New(secret))
			}}, nil
		}),
	)
	if code != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationConflict)
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %#v, want one", observations)
	}
	encoded, err := json.Marshal(observations[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte("super-secret")) || bytes.Contains(encoded, []byte(`D:\\private`)) {
		t.Fatalf("observation contains raw error: %s", encoded)
	}
}

func TestRunOperation_InternalErrorIsNotReportedBeforeFinalResult(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	stdout := &resultFailingWriter{}
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, errors.New("internal test failure")
			}}, nil
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	internal, closes, _ := recorder.snapshot()
	if len(internal) != 0 || closes != 1 {
		t.Fatalf("telemetry = internal:%d close:%d, want 0/1 when result write fails", len(internal), closes)
	}
}

func TestRunOperation_OutputPanicIsContainedWithoutPrematureTelemetry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	stdout := &resultPanickingWriter{}
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "version"}, recorder, stdout)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	internal, closes, _ := recorder.snapshot()
	if len(internal) != 0 || closes != 1 {
		t.Fatalf("telemetry = internal:%d close:%d, want 0/1 when result writer panics", len(internal), closes)
	}
	if strings.Contains(stdout.String(), "secret result writer panic") {
		t.Fatal("raw output panic leaked to stdout")
	}
}

func TestRunOperation_HelloVersionPanicReportsAfterFinalResult(t *testing.T) {
	var resultWritten atomic.Bool
	recorder := &telemetryRecorderSpy{resultWritten: &resultWritten}
	var stdout resultTrackingWriter
	stdout.resultWritten = &resultWritten
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithVersionSource(func(context.Context) (version.Info, error) {
			panic("secret version panic")
		}),
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, nil
			}}, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	internal, closes, beforeResult := recorder.snapshot()
	if len(internal) != 1 || !internal[0].Panic || closes != 1 || beforeResult {
		t.Fatalf("telemetry = internal:%d panic:%v close:%d beforeResult:%v, want 1/true/1/false", len(internal), len(internal) == 1 && internal[0].Panic, closes, beforeResult)
	}
	assertPanicFrameModules(t, internal[0].PanicFrames)
	if strings.Contains(stdout.String(), "secret version panic") {
		t.Fatal("raw version panic leaked to stdout")
	}
}

func TestRunOperation_CancelledDoesNotReportSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	var stdout bytes.Buffer
	code := Execute(ctx, []string{"--output", "ndjson", "version"}, IO{In: strings.NewReader(""), Out: &stdout, Err: io.Discard},
		WithCWD(t.TempDir()), WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }))
	if code != protocol.ExitCodeOperationCancelled {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationCancelled)
	}
	internal, closes, _ := recorder.snapshot()
	if len(internal) != 0 || closes != 1 {
		t.Fatalf("telemetry = internal:%d close:%d, want 0/1", len(internal), closes)
	}
}

func TestRunOperation_DisabledSkipsTelemetryFactory(t *testing.T) {
	var factoryCalls atomic.Int32
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	t.Setenv("AUTO_MAS_SENTRY_DSN", "https://public@example.invalid/1")
	var stdout bytes.Buffer
	code := Execute(context.Background(), []string{"--output", "ndjson", "version"}, IO{In: strings.NewReader(""), Out: &stdout, Err: io.Discard},
		WithCWD(t.TempDir()), WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder {
			factoryCalls.Add(1)
			return &telemetryRecorderSpy{}
		}))
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("telemetry factory calls = %d, want 0 when disabled", got)
	}
}

func TestRunOperation_AllCommandPathsCloseTelemetryOnCancellation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "version", args: []string{"version"}},
		{name: "doctor", args: []string{"doctor"}},
		{name: "bootstrap", args: []string{"bootstrap", "--version", "v5.4.0"}},
		{name: "workspace check", args: []string{"workspace", "check"}},
		{name: "workspace sync", args: []string{"workspace", "sync", "--version", "v5.4.0"}},
		{name: "environment check", args: []string{"environment", "check"}},
		{name: "environment ensure", args: []string{"environment", "ensure"}},
		{name: "environment repair", args: []string{"environment", "repair"}},
		{name: "dependencies check", args: []string{"dependencies", "check"}},
		{name: "dependencies sync", args: []string{"dependencies", "sync"}},
		{name: "dependencies rebuild", args: []string{"dependencies", "rebuild"}},
		{name: "backend managed", args: []string{"backend", "supervise", "--mode", "managed"}},
		{name: "backend development", args: []string{"backend", "supervise", "--mode", "development", "--repo", "source"}},
		{name: "repair", args: []string{"repair"}},
		{name: "cleanup", args: []string{"cleanup"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &telemetryRecorderSpy{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
			args := append([]string{"--output", "ndjson"}, test.args...)
			var stdout bytes.Buffer
			code := Execute(ctx, args, IO{In: strings.NewReader(""), Out: &stdout, Err: io.Discard},
				WithCWD(t.TempDir()), WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }))
			if code != protocol.ExitCodeOperationCancelled {
				t.Fatalf("exit code = %d, want %d; stdout=%s", code, protocol.ExitCodeOperationCancelled, stdout.String())
			}
			internal, closes, _ := recorder.snapshot()
			if len(internal) != 0 || closes != 1 {
				t.Fatalf("telemetry = internal:%d close:%d, want 0/1", len(internal), closes)
			}
			events := parseNDJSON(t, stdout.String())
			if got := eventType(events[len(events)-1]); got != string(protocol.TypeResult) {
				t.Fatalf("last event type = %q, want result", got)
			}
		})
	}
}

func TestRunOperation_FlushTimeoutDoesNotChangeExitCode(t *testing.T) {
	recorder := &telemetryRecorderSpy{closeWaits: true}
	var stdout bytes.Buffer
	started := time.Now()
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "version"}, recorder, &stdout)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("telemetry timeout took %s, want under 1s", elapsed)
	}
	_, closes, _ := recorder.snapshot()
	if closes != 1 {
		t.Fatalf("close calls = %d, want 1", closes)
	}
}

func TestBackendSupervise_TelemetryClosesOnShutdown(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	const commandID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	input := strings.NewReader(`{"protocol":1,"command":"shutdown","commandId":"` + commandID + `"}` + "\n")
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	var stdout bytes.Buffer
	code := Execute(context.Background(), []string{"--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: input, Out: &stdout, Err: io.Discard}, WithCWD(t.TempDir()),
		WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }),
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(_ context.Context, request backend.Request) error {
				command, err := request.Control.Receive(context.Background())
				if err != nil {
					return err
				}
				if command.Command != protocol.ControlShutdown {
					return errors.New("unexpected control command")
				}
				request.BeforeShutdown(command.CommandID)
				return nil
			}), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	_, closes, _ := recorder.snapshot()
	if closes != 1 {
		t.Fatalf("close calls = %d, want 1", closes)
	}
	events := parseNDJSON(t, stdout.String())
	if got := eventType(events[len(events)-1]); got != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", got)
	}
}

func TestRunOperation_PanicReportsSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) { panic("secret panic value") }}, nil
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	internal, _, _ := recorder.snapshot()
	if len(internal) != 1 || !internal[0].Panic {
		t.Fatalf("panic observations = %#v, want one panic", internal)
	}
	assertPanicFrameModules(t, internal[0].PanicFrames)
}

func assertPanicFrameModules(t *testing.T, frames []telemetry.StackFrame) {
	t.Helper()
	const applicationPackage = "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cli"
	found := false
	for _, frame := range frames {
		if strings.HasPrefix(frame.Function, applicationPackage+".") {
			found = true
			if frame.Module != applicationPackage {
				t.Fatalf("panic frame module = %q, want %q", frame.Module, applicationPackage)
			}
		}
	}
	if !found {
		t.Fatal("panic observation has no application frame")
	}
}

func TestBackendSupervise_ControlReaderPanicReportsSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	input := &panickingControlInput{}
	var stdout, stderr bytes.Buffer
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	code := Execute(context.Background(), []string{"--output", "ndjson", "backend", "supervise", "--mode", "managed"}, IO{In: input, Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()), WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }),
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
				_, err := request.Control.Receive(ctx)
				return err
			}), nil
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	internal, closes, _ := recorder.snapshot()
	if len(internal) != 1 || !internal[0].Panic || closes != 1 {
		t.Fatalf("telemetry = internal:%d panic:%v close:%d, want 1/true/1", len(internal), len(internal) == 1 && internal[0].Panic, closes)
	}
	assertPanicFrameModules(t, internal[0].PanicFrames)
	if strings.Contains(stdout.String(), "secret stdin panic") || strings.Contains(stderr.String(), "secret stdin panic") {
		t.Fatal("raw control reader panic leaked to process output")
	}
}

type panickingControlInput struct{}

func (*panickingControlInput) Read([]byte) (int, error) { panic("secret stdin panic") }
func (*panickingControlInput) Close() error             { return nil }
