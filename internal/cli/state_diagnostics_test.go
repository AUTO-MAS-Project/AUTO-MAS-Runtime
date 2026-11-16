package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

type earlyStateFailureStore struct {
	*m5TestStateStore
	err error
}

func (s earlyStateFailureStore) ReadTransaction(context.Context, state.TransactionKind) (state.TransactionSnapshot, error) {
	return state.TransactionSnapshot{}, s.err
}

func TestBootstrap_EarlyStateFailureIsRecorded(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	root := t.TempDir()
	var calls []string
	failure := &state.WriteError{File: "mutation", Phase: state.WritePhaseRecover, RecoveryRequired: true,
		Cause: &filesystem.FileError{Operation: "classic-unlink", Path: "private-path", Err: syscall.Errno(5)}}
	store := earlyStateFailureStore{m5TestStateStore: &m5TestStateStore{calls: &calls}, err: failure}
	var stdout, stderr bytes.Buffer
	recorder := &telemetryRecorderSpy{}
	code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.8.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
		WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder { return recorder }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &calls}, nil
		}),
	)
	if code != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want STATE_WRITE_FAILED", code)
	}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(layout.RuntimeLogDir(), "bootstrap-*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("log files = %v, error = %v, want one early failure log", paths, err)
	}
	payload, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	errorsFound := 0
	for _, line := range bytes.Split(bytes.TrimSpace(payload), []byte{'\n'}) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["level"] != "error" {
			continue
		}
		errorsFound++
		details := row["details"].(map[string]any)
		if details["persistence"] == nil || details["causes"] == nil || row["operationId"] == "" {
			t.Fatalf("failure = %#v, want state facts, full cause chain and operation ID", row)
		}
	}
	if errorsFound != 1 {
		t.Fatalf("error records = %d, want 1", errorsFound)
	}
	events := parseNDJSON(t, stdout.String())
	if strings.Contains(events[len(events)-1].raw, "persistence") || strings.Contains(stdout.String(), "private-path") {
		t.Fatalf("protocol leaked diagnostics: %s", stdout.String())
	}
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].DiagnosticDetails["persistence"] == nil {
		t.Fatalf("observations = %#v, want persistence details", observations)
	}
}

func TestRecordOperationFailure_StateErrorChain(t *testing.T) {
	log := &m5TestLog{}
	binding := &workspaceLogBinding{}
	binding.Set(log)
	deps := &deps{ctx: t.Context(), opLog: binding}
	failure := stateStoreError(protocol.StageUVDownload, &state.WriteError{
		File: "mutation", Phase: state.WritePhaseFinalize, MutationApplied: true, RecoveryRequired: true,
		Cause:        &filesystem.FileError{Operation: "classic-unlink", Path: "private-path", Err: syscall.Errno(5)},
		CleanupError: errors.New("private cleanup cause"),
	})
	recordOperationFailure(deps, protocol.CodeStateWriteFailed, protocol.StageUVDownload, "状态写入失败", nil, failure)
	if len(log.records) != 1 {
		t.Fatalf("records = %d, want 1", len(log.records))
	}
	encoded, err := json.Marshal(log.records[0].details)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"classic-unlink", "private cleanup cause", `"mutationApplied":true`, `"recoveryRequired":true`} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("local details = %s, want %q", encoded, want)
		}
	}
}

func TestRunOperation_StateDiagnosticsRespectSelectedFailure(t *testing.T) {
	stateErr := &state.WriteError{File: "mutation", Phase: state.WritePhaseFinalize, MutationApplied: true}
	main := &commandError{code: protocol.CodeMutexOperationFailed, stage: protocol.StageBootstrap, message: "锁失败"}
	details := failureTelemetryDetails(errors.Join(main, stateErr), main.code, main.stage, nil)
	if details["persistence"] != nil {
		t.Fatalf("diagnostics = %#v, want no secondary state facts", details)
	}
}

type failingStateLogger struct{ m5TestLog }

func (l *failingStateLogger) Record(context.Context, logging.Level, string, map[string]any) (logging.WriteResult, error) {
	return logging.WriteResult{}, errors.New("state log write failed")
}

func TestRunOperation_StateFailureLogFallback(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	for _, test := range []struct {
		name         string
		writeFailure bool
	}{{name: "open fails"}, {name: "write fails", writeFailure: true}} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			attempts := 0
			cause := stateStoreError(protocol.StageBootstrap, &state.WriteError{File: "mutation", Phase: state.WritePhaseFinalize, Cause: errors.New("private deepest cause")})
			code := Execute(t.Context(), []string{"--app-root", t.TempDir(), "--output", "ndjson", "doctor"}, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
					return fakeDoctorService{run: func(context.Context, *protocol.Emitter) (doctor.Report, error) { return doctor.Report{}, cause }}, nil
				}),
				WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
					attempts++
					if test.writeFailure {
						return &failingStateLogger{}, nil
					}
					return nil, errors.New("state log open failed")
				}),
			)
			if code != protocol.ExitCodeOperationConflict || attempts != 1 || !strings.Contains(stderr.String(), "private deepest cause") {
				t.Fatalf("exit=%d attempts=%d stderr=%q, want unchanged exit, one log attempt and full cause", code, attempts, stderr.String())
			}
			if strings.Contains(stdout.String(), "private") {
				t.Fatal("protocol leaked the local cause")
			}
		})
	}
}

func TestLocalStateErrorChain_Bounds(t *testing.T) {
	var err error = errors.New("root")
	for range 31 {
		err = fmt.Errorf("wrap: %w", err)
	}
	values, truncated := localStateErrorChain(err)
	if len(values) != 32 || truncated {
		t.Fatalf("chain count=%d truncated=%t, want 32 and false", len(values), truncated)
	}
	err = fmt.Errorf("wrap: %w", err)
	values, truncated = localStateErrorChain(err)
	if len(values) != 32 || !truncated {
		t.Fatalf("chain count=%d truncated=%t, want 32 and true", len(values), truncated)
	}
	values, truncated = localStateErrorChain(errors.New(strings.Repeat("错", 2000)))
	if len(values) != 1 || len(values[0]) > 4096 || !utf8.ValidString(values[0]) || !truncated {
		t.Fatalf("long chain=%#v truncated=%t, want bounded UTF-8", values, truncated)
	}
}
