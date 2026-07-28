package protocol_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const testOperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type flushingBuffer struct {
	bytes.Buffer
	flushes int
}

func (b *flushingBuffer) Flush() error {
	b.flushes++
	return nil
}

func TestNewEmitterEmitsHelloFirst(t *testing.T) {
	t.Parallel()

	var output flushingBuffer
	now := time.Date(2026, 7, 28, 14, 10, 0, 123_000_000, time.FixedZone("CST", 8*60*60))

	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := processOutput.NewEmitter(
		"v1.0.0",
		"bootstrap",
		[]string{"stdin.cancel", "state.v1", "log.stream"},
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	if got := emitter.OperationID(); got != testOperationID {
		t.Fatalf("OperationID() = %q, want %q", got, testOperationID)
	}

	lines := ndjsonLines(t, output.String())
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1", len(lines))
	}

	var hello protocol.HelloEvent
	if err := json.Unmarshal([]byte(lines[0]), &hello); err != nil {
		t.Fatalf("hello is not valid JSON: %v", err)
	}

	if hello.Protocol != protocol.Version {
		t.Errorf("hello.protocol = %d, want %d", hello.Protocol, protocol.Version)
	}
	if hello.Type != protocol.TypeHello {
		t.Errorf("hello.type = %q, want %q", hello.Type, protocol.TypeHello)
	}
	if hello.Sequence != 1 {
		t.Errorf("hello.sequence = %d, want 1", hello.Sequence)
	}
	if hello.Timestamp != now.Format(time.RFC3339Nano) {
		t.Errorf("hello.timestamp = %q, want %q", hello.Timestamp, now.Format(time.RFC3339Nano))
	}
	if hello.RuntimeVersion != "v1.0.0" || hello.Command != "bootstrap" {
		t.Errorf("hello identity = (%q, %q), want (v1.0.0, bootstrap)", hello.RuntimeVersion, hello.Command)
	}
	if output.flushes != 1 {
		t.Errorf("flush count = %d, want 1", output.flushes)
	}
}

func TestNewProcessOutputStillUsesNDJSON(t *testing.T) {
	var output bytes.Buffer
	now := time.Date(2026, 7, 28, 15, 30, 0, 0, time.UTC)
	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	if _, err := processOutput.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return now }),
	); err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	want := `{"protocol":1,"type":"hello","operationId":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sequence":1,"timestamp":"2026-07-28T15:30:00Z","runtimeVersion":"v1.0.0","command":"doctor","capabilities":[]}` + "\n"
	if got := output.String(); got != want {
		t.Errorf("NDJSON output = %q, want %q", got, want)
	}
}

func TestEmitterSerializesAllEventTypesWithGlobalSequence(t *testing.T) {
	t.Parallel()

	var output flushingBuffer
	emitter := newTestEmitter(t, &output)

	events := []struct {
		name string
		emit func() error
	}{
		{
			name: "progress",
			emit: func() error {
				current, total, percent := int64(18), int64(42), 42.86
				return emitter.EmitProgress(protocol.ProgressEvent{
					Stage: protocol.StageDependenciesSync, Status: protocol.ProgressRunning,
					Current: &current, Total: &total, Percent: &percent,
					Message: "正在安装 Python 依赖",
				})
			},
		},
		{
			name: "state",
			emit: func() error {
				return emitter.EmitState(protocol.StateEvent{
					Stage: protocol.StageDependenciesSync, Status: protocol.StateSyncingEnvironment,
					Message: "正在精确同步 Python 环境", Details: map[string]any{},
				})
			},
		},
		{
			name: "log",
			emit: func() error {
				return emitter.EmitLog(protocol.LogEvent{
					Source: "backend", Stream: "stderr", Message: "Traceback\nsecond line",
				})
			},
		},
		{
			name: "warning",
			emit: func() error {
				return emitter.EmitWarning(protocol.WarningEvent{
					Code: "BACKEND_FORCE_TERMINATED", Stage: protocol.StageBackendShutdown,
					Message: "后端已被强制终止", Retryable: false,
					Remediation: []string{"open-log"}, Details: map[string]any{"exitCode": 1},
				})
			},
		},
		{
			name: "error",
			emit: func() error {
				return emitter.EmitError(protocol.ErrorEvent{
					Code: "DEPENDENCY_SYNC_FAILED", Stage: protocol.StageDependenciesSync,
					Message: "Python 依赖安装失败", Retryable: true,
					Remediation: []string{"retry-sync", "open-log"}, Details: map[string]any{"exitCode": 1},
				})
			},
		},
		{
			name: "result",
			emit: func() error {
				return emitter.EmitResult(protocol.ResultEvent{
					Success: false, Code: "DEPENDENCY_SYNC_FAILED",
					Stage: protocol.StageDependenciesSync, Status: "environment_broken",
					Message: "Python 依赖同步失败", Retryable: true,
					Remediation: []string{"retry-sync", "open-log"},
					Details:     map[string]any{"logPath": "logs/runtime/dependencies-20260728.log"},
				})
			},
		},
	}

	for _, event := range events {
		if err := event.emit(); err != nil {
			t.Fatalf("Emit%s() error = %v", event.name, err)
		}
	}

	lines := ndjsonLines(t, output.String())
	if len(lines) != 7 {
		t.Fatalf("line count = %d, want 7", len(lines))
	}

	wantTypes := []protocol.EventType{
		protocol.TypeHello,
		protocol.TypeProgress,
		protocol.TypeState,
		protocol.TypeLog,
		protocol.TypeWarning,
		protocol.TypeError,
		protocol.TypeResult,
	}
	for i, line := range lines {
		var common protocol.Common
		if err := json.Unmarshal([]byte(line), &common); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
		if common.Type != wantTypes[i] {
			t.Errorf("line %d type = %q, want %q", i+1, common.Type, wantTypes[i])
		}
		if common.Sequence != uint64(i+1) {
			t.Errorf("line %d sequence = %d, want %d", i+1, common.Sequence, i+1)
		}
		if strings.Contains(line, "\r") || strings.Contains(line, "\n") {
			t.Errorf("line %d contains an embedded newline: %q", i+1, line)
		}
	}

	if output.flushes != len(lines) {
		t.Errorf("flush count = %d, want %d", output.flushes, len(lines))
	}
}

func TestEmitterIsSafeForConcurrentEmission(t *testing.T) {
	t.Parallel()

	var output flushingBuffer
	emitter := newTestEmitter(t, &output)

	const eventCount = 100
	var wait sync.WaitGroup
	errors := make(chan error, eventCount)
	for i := range eventCount {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if err := emitter.EmitLog(protocol.LogEvent{
				Source:  "backend",
				Stream:  "stdout",
				Message: fmt.Sprintf("line-%03d", index),
			}); err != nil {
				errors <- err
			}
		}(i)
	}
	wait.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("EmitLog() error = %v", err)
	}

	lines := ndjsonLines(t, output.String())
	if len(lines) != eventCount+1 {
		t.Fatalf("line count = %d, want %d", len(lines), eventCount+1)
	}
	for i, line := range lines {
		var common protocol.Common
		if err := json.Unmarshal([]byte(line), &common); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
		if common.Sequence != uint64(i+1) {
			t.Errorf("line %d sequence = %d, want %d", i+1, common.Sequence, i+1)
		}
	}
}

func TestGeneratedOperationIDIsULID(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := processOutput.NewEmitter("v1.0.0", "doctor", nil)
	if err != nil {
		t.Fatalf("ProcessOutput.NewEmitter() error = %v", err)
	}

	const ulidPattern = `^[0-7][0-9A-HJKMNP-TV-Z]{25}$`
	if matched := regexp.MustCompile(ulidPattern).MatchString(emitter.OperationID()); !matched {
		t.Fatalf("OperationID() = %q, want a canonical ULID", emitter.OperationID())
	}
}

func TestProcessOutputRejectsSecondEmitter(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	if _, err := processOutput.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		protocol.WithOperationID(testOperationID),
	); err != nil {
		t.Fatalf("first ProcessOutput.NewEmitter() error = %v", err)
	}
	if _, err := processOutput.NewEmitter(
		"v1.0.0",
		"bootstrap",
		nil,
		protocol.WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAW"),
	); err == nil {
		t.Fatal("second ProcessOutput.NewEmitter() error = nil, want process-output ownership error")
	}
}

func newTestEmitter(t *testing.T, output *flushingBuffer) *protocol.Emitter {
	t.Helper()

	processOutput, err := protocol.NewProcessOutput(output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := processOutput.NewEmitter(
		"v1.0.0",
		"bootstrap",
		nil,
		protocol.WithOperationID(testOperationID),
	)
	if err != nil {
		t.Fatalf("ProcessOutput.NewEmitter() error = %v", err)
	}
	return emitter
}

func ndjsonLines(t *testing.T, output string) []string {
	t.Helper()

	if !strings.HasSuffix(output, "\n") {
		t.Fatalf("output does not end with a newline: %q", output)
	}
	return strings.Split(strings.TrimSuffix(output, "\n"), "\n")
}
