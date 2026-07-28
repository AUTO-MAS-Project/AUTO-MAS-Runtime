package protocol

import (
	"errors"
	"strings"
	"testing"
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
