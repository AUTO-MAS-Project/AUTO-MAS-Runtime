package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type internalFailingRenderer struct {
	errAt EventType
	err   error
}

func (r *internalFailingRenderer) RenderHello(HelloEvent) error       { return r.render(TypeHello) }
func (r *internalFailingRenderer) RenderProgress(ProgressEvent) error { return r.render(TypeProgress) }
func (r *internalFailingRenderer) RenderState(StateEvent) error       { return r.render(TypeState) }
func (r *internalFailingRenderer) RenderLog(LogEvent) error           { return r.render(TypeLog) }
func (r *internalFailingRenderer) RenderWarning(WarningEvent) error   { return r.render(TypeWarning) }
func (r *internalFailingRenderer) RenderError(ErrorEvent) error       { return r.render(TypeError) }
func (r *internalFailingRenderer) RenderResult(ResultEvent) error     { return r.render(TypeResult) }

func (r *internalFailingRenderer) render(eventType EventType) error {
	if eventType == r.errAt {
		return r.err
	}
	return nil
}

type internalCountingRenderer struct {
	delegate EventRenderer
	calls    []EventType
}

func (r *internalCountingRenderer) RenderHello(event HelloEvent) error {
	r.calls = append(r.calls, TypeHello)
	return r.delegate.RenderHello(event)
}

func (r *internalCountingRenderer) RenderProgress(event ProgressEvent) error {
	r.calls = append(r.calls, TypeProgress)
	return r.delegate.RenderProgress(event)
}

func (r *internalCountingRenderer) RenderState(event StateEvent) error {
	r.calls = append(r.calls, TypeState)
	return r.delegate.RenderState(event)
}

func (r *internalCountingRenderer) RenderLog(event LogEvent) error {
	r.calls = append(r.calls, TypeLog)
	return r.delegate.RenderLog(event)
}

func (r *internalCountingRenderer) RenderWarning(event WarningEvent) error {
	r.calls = append(r.calls, TypeWarning)
	return r.delegate.RenderWarning(event)
}

func (r *internalCountingRenderer) RenderError(event ErrorEvent) error {
	r.calls = append(r.calls, TypeError)
	return r.delegate.RenderError(event)
}

func (r *internalCountingRenderer) RenderResult(event ResultEvent) error {
	r.calls = append(r.calls, TypeResult)
	return r.delegate.RenderResult(event)
}

func TestEmitter_TerminalRejectsEveryEvent(t *testing.T) {
	var destination bytes.Buffer
	ndjson, err := NewNDJSONRenderer(&destination)
	if err != nil {
		t.Fatalf("NewNDJSONRenderer() error = %v", err)
	}
	renderer := &internalCountingRenderer{delegate: ndjson}
	output, err := NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	clockCalls := 0
	emitter, err := output.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		WithClock(func() time.Time {
			clockCalls++
			return time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
		}),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	if err := emitter.EmitResult(ResultEvent{
		Success: true,
		Code:    "OK",
		Stage:   StageDoctor,
		Status:  "succeeded",
		Message: "done",
		Details: map[string]any{},
	}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}
	if !output.terminal {
		t.Fatal("terminal = false after successful result")
	}

	baselineOutput := destination.String()
	baselineCalls := append([]EventType(nil), renderer.calls...)
	baselineSequence := output.nextSequence
	baselineClockCalls := clockCalls
	output.writeErr = errors.New("sticky error must not hide terminal rejection")

	progress := ProgressEvent{Common: Common{Sequence: 91}, Message: "progress"}
	progressBefore := progress
	state := StateEvent{Common: Common{Sequence: 92}, Message: "state"}
	stateBefore := state
	logEvent := LogEvent{Common: Common{Sequence: 93}, Message: "log"}
	logBefore := logEvent
	warning := WarningEvent{Common: Common{Sequence: 94}, Message: "warning"}
	warningBefore := warning
	errorEvent := ErrorEvent{Common: Common{Sequence: 95}, Message: "error"}
	errorBefore := errorEvent
	result := ResultEvent{Common: Common{Sequence: 96}, Message: "result"}
	resultBefore := result
	hello := &HelloEvent{Common: Common{Sequence: 97}, Command: "internal"}
	helloBefore := *hello

	tests := []struct {
		name      string
		emit      func() error
		wantError error
		unchanged func() bool
	}{
		{name: "progress", emit: func() error { return emitter.EmitProgress(progress) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(progress, progressBefore) }},
		{name: "state", emit: func() error { return emitter.EmitState(state) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(state, stateBefore) }},
		{name: "log", emit: func() error { return emitter.EmitLog(logEvent) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(logEvent, logBefore) }},
		{name: "warning", emit: func() error { return emitter.EmitWarning(warning) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(warning, warningBefore) }},
		{name: "error", emit: func() error { return emitter.EmitError(errorEvent) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(errorEvent, errorBefore) }},
		{name: "result", emit: func() error { return emitter.EmitResult(result) }, wantError: ErrResultAlreadyEmitted, unchanged: func() bool { return reflect.DeepEqual(result, resultBefore) }},
		{name: "internal hello", emit: func() error { return emitter.emit(TypeHello, hello) }, wantError: ErrEventAfterResult, unchanged: func() bool { return reflect.DeepEqual(*hello, helloBefore) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.emit(); !errors.Is(err, test.wantError) {
				t.Fatalf("rejected emit error = %v, want %v", err, test.wantError)
			}
			if !test.unchanged() {
				t.Error("rejected emit modified its input")
			}
			if got := destination.String(); got != baselineOutput {
				t.Errorf("destination changed after rejected emit: %q", got)
			}
			if !reflect.DeepEqual(renderer.calls, baselineCalls) {
				t.Errorf("renderer calls = %v, want unchanged %v", renderer.calls, baselineCalls)
			}
			if output.nextSequence != baselineSequence {
				t.Errorf("nextSequence = %d, want unchanged %d", output.nextSequence, baselineSequence)
			}
			if clockCalls != baselineClockCalls {
				t.Errorf("clock calls = %d, want unchanged %d", clockCalls, baselineClockCalls)
			}
		})
	}
}

func TestRendererFailureDoesNotAdvanceSequence(t *testing.T) {
	renderer := &internalFailingRenderer{errAt: TypeWarning, err: errors.New("renderer failed")}
	output, err := NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	if output.nextSequence != 2 {
		t.Fatalf("nextSequence after hello = %d, want 2", output.nextSequence)
	}
	if err := emitter.EmitWarning(WarningEvent{}); err == nil {
		t.Fatal("EmitWarning() error = nil, want renderer failure")
	}
	if output.nextSequence != 2 {
		t.Errorf("nextSequence after renderer failure = %d, want 2", output.nextSequence)
	}
}

func TestUnknownInternalEventIsNonStickyAndDoesNotAdvanceSequence(t *testing.T) {
	output, err := NewProcessOutputWithRenderer(&internalFailingRenderer{})
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	unknown := &internalUnknownEvent{}
	err = emitter.emit(TypeState, unknown)
	if err == nil || err.Error() != "render protocol event: unsupported event type" {
		t.Fatalf("emit unknown event error = %v, want unsupported-type error", err)
	}
	if output.writeErr != nil {
		t.Errorf("writeErr = %v, want nil after non-sticky renderer error", output.writeErr)
	}
	if output.nextSequence != 2 {
		t.Errorf("nextSequence after unsupported event = %d, want 2", output.nextSequence)
	}
	if err := emitter.EmitLog(LogEvent{Message: "after unsupported event"}); err != nil {
		t.Fatalf("EmitLog() after unsupported event error = %v", err)
	}
	if output.nextSequence != 3 {
		t.Errorf("nextSequence after subsequent log = %d, want 3", output.nextSequence)
	}
}

type internalUnknownEvent struct {
	Common
}

func (e *internalUnknownEvent) setCommon(common Common) {
	e.Common = common
}

func TestNDJSONWriteFailuresDoNotAdvanceSequence(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		failWriteAt int
		failFlushAt int
	}{
		{name: "direct write", message: strings.Repeat("x", 8192), failWriteAt: 2},
		{name: "buffered flush", message: "short", failWriteAt: 2},
		{name: "destination flush", message: "short", failFlushAt: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New(test.name + " sentinel")
			destination := &internalFailingDestination{
				err:         sentinel,
				failWriteAt: test.failWriteAt,
				failFlushAt: test.failFlushAt,
			}
			output, err := NewProcessOutput(destination)
			if err != nil {
				t.Fatalf("NewProcessOutput() error = %v", err)
			}
			emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
			if err != nil {
				t.Fatalf("NewEmitter() error = %v", err)
			}
			if output.nextSequence != 2 {
				t.Fatalf("nextSequence after hello = %d, want 2", output.nextSequence)
			}
			if err := emitter.EmitLog(LogEvent{Message: test.message}); !errors.Is(err, sentinel) {
				t.Fatalf("EmitLog() error = %v, want sentinel", err)
			}
			if output.nextSequence != 2 {
				t.Errorf("nextSequence after failure = %d, want 2", output.nextSequence)
			}
		})
	}
}

func TestHumanRendererFailureDoesNotAdvanceSequence(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		message   string
		failWrite bool
		failFlush bool
	}{
		{name: "stdout direct write", target: "stdout", message: strings.Repeat("x", 8192), failWrite: true},
		{name: "stdout buffered flush", target: "stdout", message: "short", failWrite: true},
		{name: "stdout destination flush", target: "stdout", message: "short", failFlush: true},
		{name: "stderr direct write", target: "stderr", message: strings.Repeat("x", 8192), failWrite: true},
		{name: "stderr buffered flush", target: "stderr", message: "short", failWrite: true},
		{name: "stderr destination flush", target: "stderr", message: "short", failFlush: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New(test.name + " sentinel")
			stdout := &internalFailingDestination{err: sentinel}
			stderr := &internalFailingDestination{err: sentinel}
			renderer, err := NewHumanRenderer(stdout, stderr)
			if err != nil {
				t.Fatalf("NewHumanRenderer() error = %v", err)
			}
			output, err := NewProcessOutputWithRenderer(renderer)
			if err != nil {
				t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
			}
			emitter, err := output.NewEmitter("v1.0.0", "doctor", nil, WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
			if err != nil {
				t.Fatalf("NewEmitter() error = %v", err)
			}
			if output.nextSequence != 2 {
				t.Fatalf("nextSequence after hello = %d, want 2", output.nextSequence)
			}

			destination := stdout
			if test.target == "stderr" {
				destination = stderr
			}
			if test.failWrite {
				destination.failWriteAt = destination.writes + 1
			}
			if test.failFlush {
				destination.failFlushAt = destination.flushes + 1
			}
			if test.target == "stdout" {
				err = emitter.EmitProgress(ProgressEvent{Message: test.message})
			} else {
				err = emitter.EmitWarning(WarningEvent{Message: test.message})
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("failed emit error = %v, want sentinel", err)
			}
			if output.nextSequence != 2 {
				t.Errorf("nextSequence after human renderer failure = %d, want 2", output.nextSequence)
			}
		})
	}
}

type internalFailingDestination struct {
	err         error
	writes      int
	flushes     int
	failWriteAt int
	failFlushAt int
}

func (d *internalFailingDestination) Write(data []byte) (int, error) {
	d.writes++
	if d.writes == d.failWriteAt {
		return 0, d.err
	}
	return len(data), nil
}

func (d *internalFailingDestination) Flush() error {
	d.flushes++
	if d.flushes == d.failFlushAt {
		return d.err
	}
	return nil
}
