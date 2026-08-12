package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
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

func (r *telemetryRecorderSpy) CaptureInternal(_ context.Context, observation telemetry.InternalObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultWritten != nil && !r.resultWritten.Load() {
		r.beforeResult = true
	}
	r.internal = append(r.internal, observation)
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

func TestRunOperation_ExpectedFailureDoesNotReportSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "doctor"}, recorder, &stdout,
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
				return doctor.Report{}, doctor.NewError(protocol.CodeNetworkUnavailable, protocol.StageDoctor, "网络不可用", map[string]any{}, errors.New("expected failure"))
			}}, nil
		}),
	)
	if code != protocol.ExitCodeNetworkFailure {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeNetworkFailure)
	}
	internal, _, _ := recorder.snapshot()
	if len(internal) != 0 {
		t.Fatalf("internal observations = %#v, want none", internal)
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
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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
}

func TestBackendSupervise_ControlReaderPanicReportsSentry(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	input := &panickingControlInput{}
	var stdout, stderr bytes.Buffer
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	code := Execute(context.Background(), []string{"--output", "ndjson", "backend", "supervise", "--mode", "managed"}, IO{In: input, Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()), WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }),
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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
	if strings.Contains(stdout.String(), "secret stdin panic") || strings.Contains(stderr.String(), "secret stdin panic") {
		t.Fatal("raw control reader panic leaked to process output")
	}
}

type panickingControlInput struct{}

func (*panickingControlInput) Read([]byte) (int, error) { panic("secret stdin panic") }
func (*panickingControlInput) Close() error             { return nil }
