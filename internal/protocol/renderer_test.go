package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type recordedEvent struct {
	Type  protocol.EventType
	Value any
}

type recordingRenderer struct {
	events []recordedEvent
	errAt  protocol.EventType
	err    error
}

type recordingDelegate struct {
	delegate protocol.EventRenderer
	events   []recordedEvent
}

func (r *recordingDelegate) RenderHello(event protocol.HelloEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeHello, Value: event})
	return r.delegate.RenderHello(event)
}

func (r *recordingDelegate) RenderProgress(event protocol.ProgressEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeProgress, Value: event})
	return r.delegate.RenderProgress(event)
}

func (r *recordingDelegate) RenderState(event protocol.StateEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeState, Value: event})
	return r.delegate.RenderState(event)
}

func (r *recordingDelegate) RenderLog(event protocol.LogEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeLog, Value: event})
	return r.delegate.RenderLog(event)
}

func (r *recordingDelegate) RenderWarning(event protocol.WarningEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeWarning, Value: event})
	return r.delegate.RenderWarning(event)
}

func (r *recordingDelegate) RenderError(event protocol.ErrorEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeError, Value: event})
	return r.delegate.RenderError(event)
}

func (r *recordingDelegate) RenderResult(event protocol.ResultEvent) error {
	r.events = append(r.events, recordedEvent{Type: protocol.TypeResult, Value: event})
	return r.delegate.RenderResult(event)
}

func (r *recordingRenderer) RenderHello(event protocol.HelloEvent) error {
	return r.record(protocol.TypeHello, event)
}

func (r *recordingRenderer) RenderProgress(event protocol.ProgressEvent) error {
	return r.record(protocol.TypeProgress, event)
}

func (r *recordingRenderer) RenderState(event protocol.StateEvent) error {
	return r.record(protocol.TypeState, event)
}

func (r *recordingRenderer) RenderLog(event protocol.LogEvent) error {
	return r.record(protocol.TypeLog, event)
}

func (r *recordingRenderer) RenderWarning(event protocol.WarningEvent) error {
	return r.record(protocol.TypeWarning, event)
}

func (r *recordingRenderer) RenderError(event protocol.ErrorEvent) error {
	return r.record(protocol.TypeError, event)
}

func (r *recordingRenderer) RenderResult(event protocol.ResultEvent) error {
	return r.record(protocol.TypeResult, event)
}

func (r *recordingRenderer) record(eventType protocol.EventType, event any) error {
	if r.errAt == eventType {
		return r.err
	}
	r.events = append(r.events, recordedEvent{Type: eventType, Value: event})
	return nil
}

func TestNewProcessOutputWithRendererEmitsTypedSequence(t *testing.T) {
	clockTime := time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	renderer := &recordingRenderer{}
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"v1.0.0", "doctor", []string{"state.v1"},
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return clockTime }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	current, total, percent := int64(2), int64(4), 50.0
	if err := emitter.EmitProgress(protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Total: &total, Percent: &percent, Message: "checking"}); err != nil {
		t.Fatalf("EmitProgress() error = %v", err)
	}
	if err := emitter.EmitState(protocol.StateEvent{Stage: protocol.StageDoctor, Status: protocol.StateReadyToStart, Message: "ready", Details: map[string]any{"source": "test"}}); err != nil {
		t.Fatalf("EmitState() error = %v", err)
	}
	if err := emitter.EmitLog(protocol.LogEvent{Source: "runtime", Stream: "stdout", Message: "line"}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	if err := emitter.EmitWarning(protocol.WarningEvent{Code: "WARN", Stage: protocol.StageDoctor, Message: "warning", Remediation: []string{"open-log"}, Details: map[string]any{}}); err != nil {
		t.Fatalf("EmitWarning() error = %v", err)
	}
	if err := emitter.EmitError(protocol.ErrorEvent{Code: "ERROR", Stage: protocol.StageDoctor, Message: "error", Retryable: true, Remediation: []string{"retry"}, Details: map[string]any{}}); err != nil {
		t.Fatalf("EmitError() error = %v", err)
	}
	if err := emitter.EmitResult(protocol.ResultEvent{Success: false, Code: "ERROR", Stage: protocol.StageDoctor, Status: "failed", Message: "result", Retryable: true, Remediation: []string{"retry"}, Details: map[string]any{}}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}

	wantTypes := []protocol.EventType{protocol.TypeHello, protocol.TypeProgress, protocol.TypeState, protocol.TypeLog, protocol.TypeWarning, protocol.TypeError, protocol.TypeResult}
	common := func(eventType protocol.EventType, sequence uint64) protocol.Common {
		return protocol.Common{
			Protocol:    protocol.Version,
			Type:        eventType,
			OperationID: testOperationID,
			Sequence:    sequence,
			Timestamp:   clockTime.Format(time.RFC3339Nano),
		}
	}
	wantEvents := []recordedEvent{
		{Type: protocol.TypeHello, Value: protocol.HelloEvent{Common: common(protocol.TypeHello, 1), RuntimeVersion: "v1.0.0", Command: "doctor", Capabilities: []string{"state.v1"}}},
		{Type: protocol.TypeProgress, Value: protocol.ProgressEvent{Common: common(protocol.TypeProgress, 2), Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Total: &total, Percent: &percent, Message: "checking"}},
		{Type: protocol.TypeState, Value: protocol.StateEvent{Common: common(protocol.TypeState, 3), Stage: protocol.StageDoctor, Status: protocol.StateReadyToStart, Message: "ready", Details: map[string]any{"source": "test"}}},
		{Type: protocol.TypeLog, Value: protocol.LogEvent{Common: common(protocol.TypeLog, 4), Source: "runtime", Stream: "stdout", Message: "line"}},
		{Type: protocol.TypeWarning, Value: protocol.WarningEvent{Common: common(protocol.TypeWarning, 5), Code: "WARN", Stage: protocol.StageDoctor, Message: "warning", Remediation: []string{"open-log"}, Details: map[string]any{}}},
		{Type: protocol.TypeError, Value: protocol.ErrorEvent{Common: common(protocol.TypeError, 6), Code: "ERROR", Stage: protocol.StageDoctor, Message: "error", Retryable: true, Remediation: []string{"retry"}, Details: map[string]any{}}},
		{Type: protocol.TypeResult, Value: protocol.ResultEvent{Common: common(protocol.TypeResult, 7), Success: false, Code: "ERROR", Stage: protocol.StageDoctor, Status: "failed", Message: "result", Retryable: true, Remediation: []string{"retry"}, Details: map[string]any{
			"warnings": []protocol.WarningSummary{{
				Code: "WARN", Stage: protocol.StageDoctor, Message: "warning",
				Remediation: []string{"open-log"}, Details: map[string]any{},
			}},
			"warningCount":      uint64(1),
			"warningsTruncated": false,
		}}},
	}
	if len(renderer.events) != len(wantTypes) {
		t.Fatalf("recorded events = %d, want %d", len(renderer.events), len(wantTypes))
	}
	for index, recorded := range renderer.events {
		if recorded.Type != wantTypes[index] {
			t.Errorf("event %d type = %q, want %q", index+1, recorded.Type, wantTypes[index])
		}
		common := commonFromRecordedEvent(t, recorded.Value)
		if common.Protocol != protocol.Version || common.OperationID != testOperationID || common.Timestamp != clockTime.Format(time.RFC3339Nano) || common.Sequence != uint64(index+1) || common.Type != wantTypes[index] {
			t.Errorf("event %d common = %#v", index+1, common)
		}
		if !reflect.DeepEqual(recorded, wantEvents[index]) {
			t.Errorf("event %d = %#v, want %#v", index+1, recorded, wantEvents[index])
		}
	}
}

func TestRenderersReceiveEquivalentSemanticEvents(t *testing.T) {
	var ndjsonOutput bytes.Buffer
	ndjsonRenderer, err := protocol.NewNDJSONRenderer(&ndjsonOutput)
	if err != nil {
		t.Fatalf("NewNDJSONRenderer() error = %v", err)
	}
	ndjsonRecorder := &recordingDelegate{delegate: ndjsonRenderer}
	emitRendererEquivalenceFixture(t, ndjsonRecorder)

	var humanStdout, humanStderr bytes.Buffer
	humanRenderer, err := protocol.NewHumanRenderer(&humanStdout, &humanStderr)
	if err != nil {
		t.Fatalf("NewHumanRenderer() error = %v", err)
	}
	humanRecorder := &recordingDelegate{delegate: humanRenderer}
	emitRendererEquivalenceFixture(t, humanRecorder)

	if len(ndjsonRecorder.events) != 7 || len(humanRecorder.events) != 7 {
		t.Fatalf(
			"recorded event counts = NDJSON %d, human %d, want 7 each",
			len(ndjsonRecorder.events), len(humanRecorder.events),
		)
	}
	for index := range ndjsonRecorder.events {
		if !reflect.DeepEqual(ndjsonRecorder.events[index], humanRecorder.events[index]) {
			t.Errorf(
				"semantic event %d differs:\nNDJSON: %#v\nhuman:  %#v",
				index+1, ndjsonRecorder.events[index], humanRecorder.events[index],
			)
		}
	}

	wantNDJSON := "{\"protocol\":1,\"type\":\"hello\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":1,\"timestamp\":\"2026-07-28T17:00:00Z\",\"runtimeVersion\":\"v1.2.3\",\"command\":\"bootstrap\",\"capabilities\":[\"state.v1\",\"stdin.cancel\"]}\n" +
		"{\"protocol\":1,\"type\":\"progress\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":2,\"timestamp\":\"2026-07-28T17:00:00Z\",\"stage\":\"dependencies.sync\",\"status\":\"running\",\"current\":2,\"total\":4,\"percent\":50,\"message\":\"syncing\"}\n" +
		"{\"protocol\":1,\"type\":\"state\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":3,\"timestamp\":\"2026-07-28T17:00:00Z\",\"stage\":\"backend.spawn\",\"status\":\"starting_backend\",\"message\":\"starting\",\"details\":{\"pid\":321}}\n" +
		"{\"protocol\":1,\"type\":\"log\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":4,\"timestamp\":\"2026-07-28T17:00:00Z\",\"source\":\"backend\",\"stream\":\"stdout\",\"message\":\"first\\nsecond\"}\n" +
		"{\"protocol\":1,\"type\":\"warning\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":5,\"timestamp\":\"2026-07-28T17:00:00Z\",\"code\":\"BACKEND_FORCE_TERMINATED\",\"stage\":\"backend.shutdown\",\"message\":\"forced\",\"retryable\":false,\"remediation\":[\"open-log\"],\"details\":{\"signal\":\"kill\"}}\n" +
		"{\"protocol\":1,\"type\":\"error\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":6,\"timestamp\":\"2026-07-28T17:00:00Z\",\"code\":\"DEPENDENCY_SYNC_FAILED\",\"stage\":\"dependencies.sync\",\"message\":\"sync failed\",\"retryable\":true,\"remediation\":[\"retry-sync\",\"open-log\"],\"details\":{\"attempt\":2}}\n" +
		"{\"protocol\":1,\"type\":\"result\",\"operationId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\",\"sequence\":7,\"timestamp\":\"2026-07-28T17:00:00Z\",\"success\":false,\"code\":\"DEPENDENCY_SYNC_FAILED\",\"stage\":\"dependencies.sync\",\"status\":\"environment_broken\",\"message\":\"failed\",\"retryable\":true,\"remediation\":[\"retry-sync\",\"open-log\"],\"details\":{\"cause\":\"network\",\"warningCount\":1,\"warnings\":[{\"code\":\"BACKEND_FORCE_TERMINATED\",\"stage\":\"backend.shutdown\",\"message\":\"forced\",\"retryable\":false,\"remediation\":[\"open-log\"],\"details\":{\"signal\":\"kill\"}}],\"warningsTruncated\":false}}\n"
	if got := ndjsonOutput.String(); got != wantNDJSON {
		t.Errorf("NDJSON golden = %q, want %q", got, wantNDJSON)
	}

	wantHumanStdout := "HELLO runtime=v1.2.3 command=bootstrap capabilities=state.v1,stdin.cancel\n" +
		"PROGRESS [dependencies.sync] running current=2 total=4 percent=50% — syncing\n" +
		"STATE [backend.spawn] starting_backend — starting\n" +
		"LOG [backend:stdout] — first\n" +
		"LOG [backend:stdout] | second\n"
	if got := humanStdout.String(); got != wantHumanStdout {
		t.Errorf("human stdout golden = %q, want %q", got, wantHumanStdout)
	}
	wantHumanStderr := "WARNING [backend.shutdown] BACKEND_FORCE_TERMINATED retryable=false remediation=open-log — forced\n" +
		"ERROR [dependencies.sync] DEPENDENCY_SYNC_FAILED retryable=true remediation=retry-sync,open-log — sync failed\n" +
		"RESULT success=false code=DEPENDENCY_SYNC_FAILED stage=dependencies.sync status=environment_broken retryable=true remediation=retry-sync,open-log — failed\n"
	if got := humanStderr.String(); got != wantHumanStderr {
		t.Errorf("human stderr golden = %q, want %q", got, wantHumanStderr)
	}
}

func TestNewProcessOutputWithRendererRejectsNil(t *testing.T) {
	var typedNilRenderer *recordingRenderer
	for _, renderer := range []protocol.EventRenderer{nil, typedNilRenderer} {
		if _, err := protocol.NewProcessOutputWithRenderer(renderer); err == nil {
			t.Errorf("NewProcessOutputWithRenderer(%T) error = nil, want nil-renderer error", renderer)
		}
	}
}

func TestEventRendererMethodSet(t *testing.T) {
	typeOfRenderer := reflect.TypeOf((*protocol.EventRenderer)(nil)).Elem()
	wantMethods := map[string]struct{}{
		"RenderHello": {}, "RenderProgress": {}, "RenderState": {}, "RenderLog": {},
		"RenderWarning": {}, "RenderError": {}, "RenderResult": {},
	}
	if typeOfRenderer.NumMethod() != len(wantMethods) {
		t.Fatalf("EventRenderer method count = %d, want %d", typeOfRenderer.NumMethod(), len(wantMethods))
	}
	for method := range wantMethods {
		if _, ok := typeOfRenderer.MethodByName(method); !ok {
			t.Errorf("EventRenderer missing %s", method)
		}
	}
}

func TestNDJSONConstructorsRejectNilWriters(t *testing.T) {
	var typedNilWriter *bytes.Buffer
	for _, writer := range []io.Writer{nil, typedNilWriter} {
		if _, err := protocol.NewProcessOutput(writer); err == nil {
			t.Errorf("NewProcessOutput(%T) error = nil, want nil-output error", writer)
		}
		if _, err := protocol.NewNDJSONRenderer(writer); err == nil {
			t.Errorf("NewNDJSONRenderer(%T) error = nil, want nil-output error", writer)
		}
	}
}

func TestNDJSONMarshalFailureIsNotSticky(t *testing.T) {
	var destination bytes.Buffer
	output, err := protocol.NewProcessOutput(&destination)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	if err := emitter.EmitState(protocol.StateEvent{Details: map[string]any{"bad": make(chan int)}}); err == nil || !strings.Contains(err.Error(), "encode state event:") || strings.Contains(err.Error(), "write protocol event:") {
		t.Fatalf("EmitState() error = %v, want non-sticky state encode error", err)
	}
	if err := emitter.EmitLog(protocol.LogEvent{Source: "runtime", Stream: "stdout", Message: "after failure"}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	lines := ndjsonLines(t, destination.String())
	if len(lines) != 2 || strings.Contains(destination.String(), "bad") {
		t.Fatalf("NDJSON output = %q, want hello and log only", destination.String())
	}
	var logEvent protocol.LogEvent
	if err := json.Unmarshal([]byte(lines[1]), &logEvent); err != nil {
		t.Fatalf("decode log event: %v", err)
	}
	if logEvent.Sequence != 2 {
		t.Errorf("log sequence = %d, want 2", logEvent.Sequence)
	}
}

func TestNDJSONDirectWriteFailureIsSticky(t *testing.T) {
	testNDJSONWriteFailureIsSticky(t, "direct write", strings.Repeat("x", 8192), false)
}

func TestNDJSONBufferedFlushFailureIsSticky(t *testing.T) {
	testNDJSONWriteFailureIsSticky(t, "buffered flush", "short", false)
}

func TestNDJSONDestinationFlushFailureIsSticky(t *testing.T) {
	testNDJSONWriteFailureIsSticky(t, "destination flush", "short", true)
}

func TestRendererFailureIsSticky(t *testing.T) {
	renderer := &recordingRenderer{errAt: protocol.TypeWarning, err: errors.New("renderer failed")}
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	warningErr := emitter.EmitWarning(protocol.WarningEvent{})
	if warningErr == nil || warningErr.Error() != "write protocol event: renderer failed" {
		t.Fatalf("EmitWarning() error = %v, want wrapped renderer error", warningErr)
	}
	progressErr := emitter.EmitProgress(protocol.ProgressEvent{})
	if progressErr != warningErr {
		t.Errorf("subsequent error = %v, want same error value %v", progressErr, warningErr)
	}
	if len(renderer.events) != 1 || renderer.events[0].Type != protocol.TypeHello {
		t.Errorf("renderer events = %#v, want hello only", renderer.events)
	}
}

func emitRendererEquivalenceFixture(t *testing.T, renderer protocol.EventRenderer) {
	t.Helper()
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	clockTime := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	emitter, err := output.NewEmitter(
		"v1.2.3",
		"bootstrap",
		[]string{"state.v1", "stdin.cancel"},
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return clockTime }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	current, total, percent := int64(2), int64(4), 50.0
	events := []func() error{
		func() error {
			return emitter.EmitProgress(protocol.ProgressEvent{
				Stage: protocol.StageDependenciesSync, Status: protocol.ProgressRunning,
				Current: &current, Total: &total, Percent: &percent, Message: "syncing",
			})
		},
		func() error {
			return emitter.EmitState(protocol.StateEvent{
				Stage: protocol.StageBackendSpawn, Status: protocol.StateStartingBackend,
				Message: "starting", Details: map[string]any{"pid": int64(321)},
			})
		},
		func() error {
			return emitter.EmitLog(protocol.LogEvent{Source: "backend", Stream: "stdout", Message: "first\nsecond"})
		},
		func() error {
			return emitter.EmitWarning(protocol.WarningEvent{
				Code: "BACKEND_FORCE_TERMINATED", Stage: protocol.StageBackendShutdown,
				Message: "forced", Remediation: []string{"open-log"}, Details: map[string]any{"signal": "kill"},
			})
		},
		func() error {
			return emitter.EmitError(protocol.ErrorEvent{
				Code: "DEPENDENCY_SYNC_FAILED", Stage: protocol.StageDependenciesSync,
				Message: "sync failed", Retryable: true, Remediation: []string{"retry-sync", "open-log"},
				Details: map[string]any{"attempt": int64(2)},
			})
		},
		func() error {
			return emitter.EmitResult(protocol.ResultEvent{
				Success: false, Code: "DEPENDENCY_SYNC_FAILED", Stage: protocol.StageDependenciesSync,
				Status: "environment_broken", Message: "failed", Retryable: true,
				Remediation: []string{"retry-sync", "open-log"}, Details: map[string]any{"cause": "network"},
			})
		},
	}
	for index, emit := range events {
		if err := emit(); err != nil {
			t.Fatalf("fixture event %d error = %v", index+2, err)
		}
	}
}

func commonFromRecordedEvent(t *testing.T, event any) protocol.Common {
	t.Helper()
	switch value := event.(type) {
	case protocol.HelloEvent:
		return value.Common
	case protocol.ProgressEvent:
		return value.Common
	case protocol.StateEvent:
		return value.Common
	case protocol.LogEvent:
		return value.Common
	case protocol.WarningEvent:
		return value.Common
	case protocol.ErrorEvent:
		return value.Common
	case protocol.ResultEvent:
		return value.Common
	default:
		t.Fatalf("unexpected recorded event type %T", event)
		return protocol.Common{}
	}
}

type failingWriter struct {
	err      error
	writes   int
	failOn   int
	accepted bytes.Buffer
}

func (w *failingWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes < w.failOn {
		return w.accepted.Write(data)
	}
	return 0, w.err
}

type flushFailWriter struct {
	writes    int
	flushes   int
	err       error
	failFlush int
	accepted  bytes.Buffer
}

func (w *flushFailWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.accepted.Write(data)
}

func (w *flushFailWriter) Flush() error {
	w.flushes++
	if w.flushes == w.failFlush {
		return w.err
	}
	return nil
}

func testNDJSONWriteFailureIsSticky(t *testing.T, name, message string, destinationFlush bool) {
	t.Helper()
	sentinel := fmt.Errorf("%s sentinel", name)
	var writer io.Writer
	var writes func() int
	var flushes func() int
	var contents func() string
	if destinationFlush {
		value := &flushFailWriter{err: sentinel, failFlush: 2}
		writer = value
		writes = func() int { return value.writes }
		flushes = func() int { return value.flushes }
		contents = func() string { return value.accepted.String() }
	} else {
		value := &failingWriter{err: sentinel, failOn: 2}
		writer = value
		writes = func() int { return value.writes }
		flushes = func() int { return 0 }
		contents = func() string { return value.accepted.String() }
	}

	output, err := protocol.NewProcessOutput(writer)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, protocol.WithOperationID(testOperationID))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v, want successful hello", err)
	}
	firstErr := emitter.EmitLog(protocol.LogEvent{Message: message})
	if firstErr == nil || !errors.Is(firstErr, sentinel) || firstErr.Error() != "write protocol event: "+sentinel.Error() {
		t.Fatalf("EmitLog() error = %v, want wrapped sentinel", firstErr)
	}
	lines := ndjsonLines(t, contents())
	if destinationFlush {
		if len(lines) != 2 || !strings.Contains(lines[1], message) {
			t.Fatalf("destination output = %q, want hello plus the complete failed event before destination Flush() failed", contents())
		}
	} else if len(lines) != 1 || strings.Contains(contents(), message) {
		t.Fatalf("destination output = %q, want only the successful hello", contents())
	}
	beforeWrites, beforeFlushes := writes(), flushes()
	secondErr := emitter.EmitProgress(protocol.ProgressEvent{})
	if secondErr != firstErr {
		t.Errorf("subsequent error = %v, want same error value %v", secondErr, firstErr)
	}
	if writes() != beforeWrites || flushes() != beforeFlushes {
		t.Errorf("I/O counts changed after sticky error: writes %d -> %d, flushes %d -> %d", beforeWrites, writes(), beforeFlushes, flushes())
	}
}
