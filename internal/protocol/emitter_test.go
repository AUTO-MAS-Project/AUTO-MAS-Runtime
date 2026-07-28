package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

func TestEmitter_ConcurrentResults(t *testing.T) {
	var output flushingBuffer
	emitter := newTestEmitter(t, &output)

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	for index := range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			results <- emitter.EmitResult(protocol.ResultEvent{
				Success: true,
				Code:    "OK",
				Stage:   protocol.StageDoctor,
				Status:  "succeeded",
				Message: fmt.Sprintf("result-%d", index),
				Details: map[string]any{},
			})
		}()
	}
	readyTimeout := time.NewTimer(2 * time.Second)
	defer readyTimeout.Stop()
	for range 2 {
		select {
		case <-ready:
		case <-readyTimeout.C:
			t.Fatal("concurrent EmitResult() barrier timed out")
		}
	}
	close(start)

	var succeeded, rejected int
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, protocol.ErrResultAlreadyEmitted):
				rejected++
			default:
				t.Fatalf("concurrent EmitResult() error = %v", err)
			}
		case <-timeout.C:
			t.Fatal("concurrent EmitResult() timed out")
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent result outcomes = %d success, %d rejected; want 1 each", succeeded, rejected)
	}

	lines := ndjsonLines(t, output.String())
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want hello and one result", len(lines))
	}
	var common protocol.Common
	if err := json.Unmarshal([]byte(lines[1]), &common); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if common.Type != protocol.TypeResult || common.Sequence != 2 {
		t.Fatalf("terminal common = %#v, want result sequence 2", common)
	}
}

func TestEmitter_ResultRetryAfterEncodeFailure(t *testing.T) {
	var output flushingBuffer
	emitter := newTestEmitter(t, &output)
	hello := output.String()

	event := protocol.ResultEvent{
		Common:  protocol.Common{Sequence: 99},
		Success: true,
		Code:    "OK",
		Stage:   protocol.StageDoctor,
		Status:  "succeeded",
		Message: "done",
		Details: map[string]any{"invalid": func() {}},
	}
	if err := emitter.EmitResult(event); err == nil || !strings.Contains(err.Error(), "encode result event") {
		t.Fatalf("first EmitResult() error = %v, want encode error", err)
	}
	if got := output.String(); got != hello {
		t.Fatalf("output after encode failure = %q, want unchanged %q", got, hello)
	}
	if event.Common.Sequence != 99 {
		t.Fatalf("input sequence after encode failure = %d, want 99", event.Common.Sequence)
	}

	event.Details = map[string]any{"attempt": 2}
	if err := emitter.EmitResult(event); err != nil {
		t.Fatalf("retry EmitResult() error = %v", err)
	}
	if err := emitter.EmitResult(protocol.ResultEvent{}); !errors.Is(err, protocol.ErrResultAlreadyEmitted) {
		t.Fatalf("EmitResult() after successful retry error = %v, want ErrResultAlreadyEmitted", err)
	}

	lines := ndjsonLines(t, output.String())
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want hello and one result", len(lines))
	}
	var result protocol.ResultEvent
	if err := json.Unmarshal([]byte(lines[1]), &result); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if result.Sequence != 2 || result.Details["attempt"] != float64(2) {
		t.Fatalf("retried result = %#v, want sequence 2 and attempt 2", result)
	}
}

type countingJSONMarshaler struct {
	calls *int
}

func (m countingJSONMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls = *m.calls + 1
	return []byte(fmt.Sprintf(`{"call":%d}`, *m.calls)), nil
}

type mutatingWarningRenderer struct {
	recordingRenderer
	received protocol.WarningSummary
}

func (r *mutatingWarningRenderer) RenderWarning(event protocol.WarningEvent) error {
	r.received = protocol.WarningSummary{
		Code:        event.Code,
		Stage:       event.Stage,
		Message:     event.Message,
		Retryable:   event.Retryable,
		Remediation: append([]string(nil), event.Remediation...),
		Details:     cloneTestJSONObject(event.Details),
	}
	if len(event.Remediation) != 0 {
		event.Remediation[0] = "renderer-mutated"
	}
	if nested, ok := event.Details["nested"].(map[string]any); ok {
		nested["value"] = "renderer-mutated"
	}
	return r.record(protocol.TypeWarning, event)
}

func TestEmitter_WarningSummary(t *testing.T) {
	renderer := &mutatingWarningRenderer{}
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		protocol.WithOperationID(testOperationID),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	marshalCalls := 0
	remediation := []string{"open-log"}
	nested := map[string]any{"value": "original"}
	details := map[string]any{
		"nested":   nested,
		"volatile": countingJSONMarshaler{calls: &marshalCalls},
	}
	warning := protocol.WarningEvent{
		Common:      protocol.Common{Sequence: 99},
		Code:        "WARN",
		Stage:       protocol.StageDoctor,
		Message:     "warning",
		Retryable:   true,
		Remediation: remediation,
		Details:     details,
	}
	warningBefore := warning
	if err := emitter.EmitWarning(warning); err != nil {
		t.Fatalf("EmitWarning() error = %v", err)
	}
	if marshalCalls != 1 {
		t.Fatalf("MarshalJSON() calls = %d, want 1", marshalCalls)
	}
	if warning.Common.Sequence != warningBefore.Common.Sequence {
		t.Fatalf("input warning sequence = %d, want unchanged %d", warning.Common.Sequence, warningBefore.Common.Sequence)
	}

	remediation[0] = "caller-mutated"
	nested["value"] = "caller-mutated"
	details["added"] = true
	resultDetails := map[string]any{
		"keep":              "value",
		"warnings":          "forged",
		"warningCount":      -1,
		"warningsTruncated": true,
	}
	resultDetailsBefore := map[string]any{
		"keep":              "value",
		"warnings":          "forged",
		"warningCount":      -1,
		"warningsTruncated": true,
	}
	if err := emitter.EmitResult(protocol.ResultEvent{
		Success: true,
		Code:    "OK",
		Stage:   protocol.StageDoctor,
		Status:  "succeeded",
		Message: "done",
		Details: resultDetails,
	}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}
	if !reflect.DeepEqual(resultDetails, resultDetailsBefore) {
		t.Fatalf("input result details = %#v, want unchanged %#v", resultDetails, resultDetailsBefore)
	}

	recordedResult := lastRecordedResult(t, renderer.events)
	summaries, ok := recordedResult.Details["warnings"].([]protocol.WarningSummary)
	if !ok {
		t.Fatalf("result warnings type = %T, want []protocol.WarningSummary", recordedResult.Details["warnings"])
	}
	if len(summaries) != 1 {
		t.Fatalf("warning summaries = %d, want 1", len(summaries))
	}
	wantSummary := protocol.WarningSummary{
		Code:        "WARN",
		Stage:       protocol.StageDoctor,
		Message:     "warning",
		Retryable:   true,
		Remediation: []string{"open-log"},
		Details: map[string]any{
			"nested":   map[string]any{"value": "original"},
			"volatile": map[string]any{"call": json.Number("1")},
		},
	}
	if !reflect.DeepEqual(summaries[0], wantSummary) {
		t.Fatalf("warning summary = %#v, want %#v", summaries[0], wantSummary)
	}
	if !reflect.DeepEqual(renderer.received, wantSummary) {
		t.Fatalf("canonical warning = %#v, want same business fields as summary %#v", renderer.received, wantSummary)
	}
	if got := recordedResult.Details["warningCount"]; got != uint64(1) {
		t.Errorf("warningCount = %#v, want uint64(1)", got)
	}
	if got := recordedResult.Details["warningsTruncated"]; got != false {
		t.Errorf("warningsTruncated = %#v, want false", got)
	}
	if got := recordedResult.Details["keep"]; got != "value" {
		t.Errorf("preserved detail = %#v, want value", got)
	}

	encodedSummary, err := json.Marshal(summaries[0])
	if err != nil {
		t.Fatalf("Marshal(summary) error = %v", err)
	}
	var summaryFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedSummary, &summaryFields); err != nil {
		t.Fatalf("Unmarshal(summary fields) error = %v", err)
	}
	wantFields := []string{"code", "stage", "message", "retryable", "remediation", "details"}
	if len(summaryFields) != len(wantFields) {
		t.Fatalf("summary field count = %d, want %d: %s", len(summaryFields), len(wantFields), encodedSummary)
	}
	for _, field := range wantFields {
		if _, ok := summaryFields[field]; !ok {
			t.Errorf("summary missing field %q: %s", field, encodedSummary)
		}
	}
	for _, forbidden := range []string{"protocol", "type", "operationId", "sequence", "timestamp"} {
		if _, ok := summaryFields[forbidden]; ok {
			t.Errorf("summary unexpectedly contains Common field %q: %s", forbidden, encodedSummary)
		}
	}

	var retryDestination bytes.Buffer
	retryOutput, err := protocol.NewProcessOutput(&retryDestination)
	if err != nil {
		t.Fatalf("retry NewProcessOutput() error = %v", err)
	}
	retryEmitter, err := retryOutput.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
	if err != nil {
		t.Fatalf("retry NewEmitter() error = %v", err)
	}
	if err := retryEmitter.EmitWarning(protocol.WarningEvent{
		Code: "RETRY_WARN", Stage: protocol.StageDoctor, Message: "retained", Details: map[string]any{},
	}); err != nil {
		t.Fatalf("retry EmitWarning() error = %v", err)
	}
	beforeFailedResult := retryDestination.String()
	if err := retryEmitter.EmitResult(protocol.ResultEvent{
		Success: true, Code: "OK", Stage: protocol.StageDoctor, Status: "succeeded",
		Message: "invalid", Details: map[string]any{"invalid": func() {}},
	}); err == nil || !strings.Contains(err.Error(), "encode result event") {
		t.Fatalf("failed EmitResult() error = %v, want encode result event error", err)
	}
	if got := retryDestination.String(); got != beforeFailedResult {
		t.Fatalf("output after failed result = %q, want unchanged %q", got, beforeFailedResult)
	}
	if err := retryEmitter.EmitResult(protocol.ResultEvent{
		Success: true, Code: "OK", Stage: protocol.StageDoctor, Status: "succeeded",
		Message: "retried", Details: map[string]any{},
	}); err != nil {
		t.Fatalf("retried EmitResult() error = %v", err)
	}
	retriedResult := decodeLastResultUseNumber(t, retryDestination.String())
	if retriedResult.Details["warningCount"] != json.Number("1") {
		t.Fatalf("retried result warningCount = %#v, want 1", retriedResult.Details["warningCount"])
	}
	if summaries, ok := retriedResult.Details["warnings"].([]any); !ok || len(summaries) != 1 {
		t.Fatalf("retried result warnings = %#v, want one retained summary", retriedResult.Details["warnings"])
	}
}

func TestEmitter_WarningSummaryLimit(t *testing.T) {
	for _, warningCount := range []int{0, 1, protocol.MaxResultWarningSummaries, protocol.MaxResultWarningSummaries + 1} {
		t.Run(fmt.Sprintf("count_%d", warningCount), func(t *testing.T) {
			var destination bytes.Buffer
			output, err := protocol.NewProcessOutput(&destination)
			if err != nil {
				t.Fatalf("NewProcessOutput() error = %v", err)
			}
			emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
			if err != nil {
				t.Fatalf("NewEmitter() error = %v", err)
			}
			for index := range warningCount {
				if err := emitter.EmitWarning(protocol.WarningEvent{
					Code:        fmt.Sprintf("WARN_%03d", index),
					Stage:       protocol.StageDoctor,
					Message:     "warning",
					Remediation: []string{},
					Details:     map[string]any{"index": index},
				}); err != nil {
					t.Fatalf("EmitWarning(%d) error = %v", index, err)
				}
			}
			if err := emitter.EmitResult(protocol.ResultEvent{
				Success: true,
				Code:    "OK",
				Stage:   protocol.StageDoctor,
				Status:  "succeeded",
				Message: "done",
				Details: map[string]any{
					"warnings":          "forged",
					"warningCount":      "forged",
					"warningsTruncated": "forged",
					"keep":              true,
				},
			}); err != nil {
				t.Fatalf("EmitResult() error = %v", err)
			}

			result := decodeLastResultUseNumber(t, destination.String())
			if result.Details["keep"] != true {
				t.Errorf("keep = %#v, want true", result.Details["keep"])
			}
			if warningCount == 0 {
				for _, key := range []string{"warnings", "warningCount", "warningsTruncated"} {
					if _, ok := result.Details[key]; ok {
						t.Errorf("zero-warning result contains reserved key %q: %#v", key, result.Details[key])
					}
				}
				return
			}
			summaries, ok := result.Details["warnings"].([]any)
			if !ok {
				t.Fatalf("warnings type = %T, want []any", result.Details["warnings"])
			}
			wantStored := min(warningCount, protocol.MaxResultWarningSummaries)
			if len(summaries) != wantStored {
				t.Errorf("stored warnings = %d, want %d", len(summaries), wantStored)
			}
			if got := result.Details["warningCount"]; got != json.Number(fmt.Sprint(warningCount)) {
				t.Errorf("warningCount = %#v, want %d", got, warningCount)
			}
			if got := result.Details["warningsTruncated"]; got != (warningCount > protocol.MaxResultWarningSummaries) {
				t.Errorf("warningsTruncated = %#v, want %v", got, warningCount > protocol.MaxResultWarningSummaries)
			}
		})
	}
}

type blockingRecordingRenderer struct {
	recordingRenderer
	blockType protocol.EventType
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *blockingRecordingRenderer) block(eventType protocol.EventType) {
	if eventType != r.blockType {
		return
	}
	r.once.Do(func() { close(r.entered) })
	<-r.release
}

func (r *blockingRecordingRenderer) RenderWarning(event protocol.WarningEvent) error {
	r.block(protocol.TypeWarning)
	return r.record(protocol.TypeWarning, event)
}

func (r *blockingRecordingRenderer) RenderResult(event protocol.ResultEvent) error {
	r.block(protocol.TypeResult)
	return r.record(protocol.TypeResult, event)
}

func TestEmitter_WarningResultRace(t *testing.T) {
	t.Run("warning linearizes first", func(t *testing.T) {
		renderer, emitter := newBlockingEmitter(t, protocol.TypeWarning)
		warningResult := make(chan error, 1)
		go func() {
			warningResult <- emitter.EmitWarning(protocol.WarningEvent{
				Code: "WARN", Stage: protocol.StageDoctor, Message: "warning", Details: map[string]any{},
			})
		}()
		waitClosed(t, renderer.entered, "warning renderer entry")

		resultReady := make(chan struct{})
		resultResult := make(chan error, 1)
		go func() {
			close(resultReady)
			resultResult <- emitter.EmitResult(protocol.ResultEvent{
				Success: true, Code: "OK", Stage: protocol.StageDoctor,
				Status: "succeeded", Message: "done", Details: map[string]any{},
			})
		}()
		waitClosed(t, resultReady, "result goroutine readiness")
		close(renderer.release)
		if err := waitError(t, warningResult, "warning completion"); err != nil {
			t.Fatalf("EmitWarning() error = %v", err)
		}
		if err := waitError(t, resultResult, "result completion"); err != nil {
			t.Fatalf("EmitResult() error = %v", err)
		}
		result := lastRecordedResult(t, renderer.events)
		if result.Details["warningCount"] != uint64(1) {
			t.Fatalf("result warningCount = %#v, want 1", result.Details["warningCount"])
		}
	})

	t.Run("result linearizes first", func(t *testing.T) {
		renderer, emitter := newBlockingEmitter(t, protocol.TypeResult)
		resultResult := make(chan error, 1)
		go func() {
			resultResult <- emitter.EmitResult(protocol.ResultEvent{
				Success: true, Code: "OK", Stage: protocol.StageDoctor,
				Status: "succeeded", Message: "done", Details: map[string]any{},
			})
		}()
		waitClosed(t, renderer.entered, "result renderer entry")

		warningReady := make(chan struct{})
		warningResult := make(chan error, 1)
		go func() {
			close(warningReady)
			warningResult <- emitter.EmitWarning(protocol.WarningEvent{
				Code: "WARN", Stage: protocol.StageDoctor, Message: "warning", Details: map[string]any{},
			})
		}()
		waitClosed(t, warningReady, "warning goroutine readiness")
		close(renderer.release)
		if err := waitError(t, resultResult, "result completion"); err != nil {
			t.Fatalf("EmitResult() error = %v", err)
		}
		if err := waitError(t, warningResult, "warning completion"); !errors.Is(err, protocol.ErrEventAfterResult) {
			t.Fatalf("EmitWarning() error = %v, want ErrEventAfterResult", err)
		}
		result := lastRecordedResult(t, renderer.events)
		for _, key := range []string{"warnings", "warningCount", "warningsTruncated"} {
			if _, ok := result.Details[key]; ok {
				t.Errorf("result-first details contain %q", key)
			}
		}
	})
}

func newBlockingEmitter(t *testing.T, blockType protocol.EventType) (*blockingRecordingRenderer, *protocol.Emitter) {
	t.Helper()
	renderer := &blockingRecordingRenderer{
		blockType: blockType,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	return renderer, emitter
}

func waitClosed(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitError(t *testing.T, result <-chan error, description string) error {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func lastRecordedResult(t *testing.T, events []recordedEvent) protocol.ResultEvent {
	t.Helper()
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == protocol.TypeResult {
			result, ok := events[index].Value.(protocol.ResultEvent)
			if !ok {
				t.Fatalf("recorded result type = %T", events[index].Value)
			}
			return result
		}
	}
	t.Fatal("renderer did not receive a result")
	return protocol.ResultEvent{}
}

func decodeLastResultUseNumber(t *testing.T, output string) protocol.ResultEvent {
	t.Helper()
	lines := ndjsonLines(t, output)
	var result protocol.ResultEvent
	decoder := json.NewDecoder(strings.NewReader(lines[len(lines)-1]))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return result
}

func cloneTestJSONObject(object map[string]any) map[string]any {
	if object == nil {
		return nil
	}
	cloned := make(map[string]any, len(object))
	for key, value := range object {
		switch value := value.(type) {
		case map[string]any:
			cloned[key] = cloneTestJSONObject(value)
		case []any:
			items := make([]any, len(value))
			copy(items, value)
			cloned[key] = items
		default:
			cloned[key] = value
		}
	}
	return cloned
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
