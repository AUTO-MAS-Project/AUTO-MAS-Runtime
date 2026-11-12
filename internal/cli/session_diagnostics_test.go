package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func diagnosticSessionFixture(t *testing.T) (*deps, *bytes.Buffer, *time.Time) {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	values, err := applyOptions()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	values.clock = func() time.Time { return now }
	out := &bytes.Buffer{}
	d := &deps{ctx: t.Context(), io: IO{In: strings.NewReader(""), Out: out, Err: &bytes.Buffer{}}, options: values,
		global: globalOptions{layout: layout, output: outputNDJSON}, opLog: &workspaceLogBinding{}}
	return d, out, &now
}

func expectedSessionFileSystem(t *testing.T, observations []telemetry.InternalObservation) any {
	t.Helper()
	if len(observations) != 1 {
		t.Fatalf("got %d observations, want 1", len(observations))
	}
	format := observations[0].DiagnosticDetails["fileSystem"]
	switch format {
	case "NTFS", "ReFS", "exFAT", "FAT", "FAT32", "unknown":
		return format
	}
	t.Fatalf("got filesystem %v, want known diagnostic format", format)
	return nil
}

func readSessionRecords(t *testing.T, d *deps) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(d.global.layout.RuntimeLogDir(), "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			var row map[string]any
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func TestSessionDiagnostics_Lifecycle(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, out, now := diagnosticSessionFixture(t)
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(_ context.Context, e *protocol.Emitter) (sessionSuccess, error) {
		if err := e.EmitProgress(protocol.ProgressEvent{Stage: protocol.StageUVDownload, Status: protocol.ProgressRunning}); err != nil {
			return sessionSuccess{}, err
		}
		*now = now.Add(2 * time.Second)
		if err := e.EmitState(protocol.StateEvent{Stage: protocol.StagePythonInstall, Status: protocol.StatePreparingPython}); err != nil {
			return sessionSuccess{}, err
		}
		*now = now.Add(3 * time.Second)
		return sessionSuccess{message: "完成", details: map[string]any{"changed": true, "commit": "abcdef", "secret": "private"}}, nil
	})
	if code != 0 {
		t.Fatalf("got exit %d, want 0", code)
	}
	events := parseNDJSON(t, out.String())
	id := events[0].object["operationId"]
	counts := map[string]int{}
	stages := map[string]float64{}
	for _, row := range readSessionRecords(t, d) {
		details, _ := row["details"].(map[string]any)
		operation, _ := details["operation"].(string)
		counts[operation]++
		if operation != "" && row["operationId"] != id {
			t.Fatalf("got ID %v, want %v", row["operationId"], id)
		}
		if operation == "operation.start" && details["runtimeVersion"] == nil {
			t.Fatal("missing runtime version")
		}
		if operation == "stage.end" {
			stages[details["stage"].(string)] = details["durationMs"].(float64)
		}
		if operation == "operation.end" && (details["success"] != true || details["durationMs"] != float64(5000) || strings.Contains(string(mustJSON(t, details)), "private")) {
			t.Fatalf("got end %#v, want safe success and 5000 ms", details)
		}
		if operation == "operation.end" && details["summary"].(map[string]any)["changed"] != true {
			t.Fatal("missing success change summary")
		}
	}
	if counts["operation.start"] != 1 || counts["operation.end"] != 1 || counts["state.change"] != 1 || stages["uv.download"] != 2000 || stages["python.install"] != 3000 {
		t.Fatalf("got counts=%v stages=%v, want complete lifecycle", counts, stages)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSessionDiagnostics_EarlyFailure(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, _, _ := diagnosticSessionFixture(t)
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(context.Context, *protocol.Emitter) (sessionSuccess, error) {
		return sessionSuccess{}, &commandError{code: protocol.CodeMutexOperationFailed, stage: protocol.StageBootstrap, message: "锁失败", cause: errors.New("private local cause")}
	})
	if code != exitCodeFor(protocol.CodeMutexOperationFailed) {
		t.Fatalf("got exit %d, want mutex failure", code)
	}
	found := false
	for _, row := range readSessionRecords(t, d) {
		if row["level"] == "error" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing early failure record")
	}
}

func TestSessionDiagnostics_ReadOnly(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, _, _ := diagnosticSessionFixture(t)
	code := runOperation(t.Context(), d, "workspace check", protocol.StageWorkspaceCheck, func(context.Context, *protocol.Emitter) (sessionSuccess, error) { return sessionSuccess{}, nil })
	if code != 0 || len(readSessionRecords(t, d)) != 0 {
		t.Fatal("read-only success created logs")
	}
}

func TestSessionDiagnostics_Concurrent(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, _, _ := diagnosticSessionFixture(t)
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(_ context.Context, e *protocol.Emitter) (sessionSuccess, error) {
		var wg sync.WaitGroup
		failures := make(chan error, 20)
		for range 20 {
			wg.Go(func() {
				failures <- e.EmitState(protocol.StateEvent{Stage: protocol.StageUVDownload, Status: protocol.StatePreparingUV})
			})
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				return sessionSuccess{}, err
			}
		}
		return sessionSuccess{}, nil
	})
	count := 0
	var previous float64
	for _, row := range readSessionRecords(t, d) {
		details, _ := row["details"].(map[string]any)
		if details["operation"] == "state.change" {
			sequence := details["sequence"].(float64)
			if sequence <= previous {
				t.Fatal("state log order differs from protocol")
			}
			previous = sequence
			count++
		}
	}
	if code != 0 || count != 20 {
		t.Fatalf("got code=%d states=%d, want 0 and 20", code, count)
	}
}

func TestSessionDiagnostics_TelemetryReceipt(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	d, out, _ := diagnosticSessionFixture(t)
	recorder := &telemetryRecorderSpy{}
	d.options.telemetryFactory = func(telemetry.Config) telemetry.Recorder { return recorder }
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(context.Context, *protocol.Emitter) (sessionSuccess, error) {
		return sessionSuccess{}, &commandError{code: protocol.CodeMutexOperationFailed, stage: protocol.StageBootstrap, message: "锁失败"}
	})
	if code != exitCodeFor(protocol.CodeMutexOperationFailed) {
		t.Fatalf("got exit %d, want mutex failure", code)
	}
	events := parseNDJSON(t, out.String())
	observations, closes, _ := recorder.snapshot()
	if len(observations) != 1 || closes != 1 || observations[0].OperationID != events[0].object["operationId"] || observations[0].DiagnosticDetails["fileSystem"] == nil {
		t.Fatalf("got observations %#v closes %d, want correlated format", observations, closes)
	}
	found := false
	for _, row := range readSessionRecords(t, d) {
		details, _ := row["details"].(map[string]any)
		if details["operation"] == "telemetry.accepted" && details["sentryEventId"] == "0123456789abcdef0123456789abcdef" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing local event receipt")
	}
}

func TestSessionTelemetry_EnvironmentIsSanitized(t *testing.T) {
	recorder := &telemetryRecorderSpy{}
	failure := &commandError{code: protocol.CodeMutexOperationFailed, stage: protocol.StageBootstrap, message: "锁失败"}
	recordSessionTelemetry(recorder, "bootstrap", protocol.StageBootstrap, "dev", failure, false, nil, "01J00000000000000000000000", "private-format")
	observations, _, _ := recorder.snapshot()
	if len(observations) != 1 || observations[0].DiagnosticDetails["fileSystem"] != nil {
		t.Fatalf("got %#v, want no unapproved filesystem format", observations)
	}
}

func TestSessionDiagnostics_UnavailableLogPreservesResult(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, out, _ := diagnosticSessionFixture(t)
	if err := os.WriteFile(filepath.Join(d.global.layout.AppRoot(), "debug"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(context.Context, *protocol.Emitter) (sessionSuccess, error) {
		return sessionSuccess{message: "完成"}, nil
	})
	events := parseNDJSON(t, out.String())
	if code != 0 || eventType(events[len(events)-1]) != "result" || events[len(events)-1].object["success"] != true {
		t.Fatal("observation log failure changed protocol result")
	}
}

type failingDiagnosticOutput struct {
	bytes.Buffer
	broken bool
}

func (w *failingDiagnosticOutput) Write(value []byte) (int, error) {
	if w.broken {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(value)
}

func TestSessionDiagnostics_BrokenOutputStillEnds(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	d, _, _ := diagnosticSessionFixture(t)
	out := &failingDiagnosticOutput{}
	d.io.Out = out
	code := runOperation(t.Context(), d, "bootstrap", protocol.StageBootstrap, func(_ context.Context, e *protocol.Emitter) (sessionSuccess, error) {
		if err := e.EmitProgress(protocol.ProgressEvent{Stage: protocol.StageUVDownload, Status: protocol.ProgressRunning}); err != nil {
			return sessionSuccess{}, err
		}
		out.broken = true
		if err := e.EmitProgress(protocol.ProgressEvent{Stage: protocol.StageUVDownload, Status: protocol.ProgressRunning}); err != nil {
			return sessionSuccess{}, err
		}
		return sessionSuccess{}, nil
	})
	ends := 0
	for _, row := range readSessionRecords(t, d) {
		details, _ := row["details"].(map[string]any)
		if details["operation"] == "operation.end" {
			ends++
			if details["terminalWritten"] != false {
				t.Fatal("broken output marked terminal written")
			}
		}
	}
	if code != exitCodeFor(protocol.CodeOutputWriteFailed) || ends != 1 {
		t.Fatalf("got code=%d ends=%d, want output failure with one local end", code, ends)
	}
}
