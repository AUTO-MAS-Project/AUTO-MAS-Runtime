package contracttest_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

const demoOperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

var demoTime = time.Date(2026, 7, 28, 8, 0, 0, 123456789, time.UTC)

func TestContract_RegisterDemo(t *testing.T) {
	contracttest.Register(t, "doctor", demoRunner)
}

func demoRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
	t.Helper()

	var stdout bytes.Buffer
	output, err := protocol.NewProcessOutput(&stdout)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"0.1.0",
		"doctor",
		[]string{},
		protocol.WithOperationID(demoOperationID),
		protocol.WithClock(func() time.Time { return demoTime }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	switch terminal {
	case contracttest.TerminalSuccess:
		warning := mustNewWarningEvent(
			t,
			protocol.CodeInvalidControlCommand,
			"invalid control command ignored",
			map[string]any{"command": "unsupported"},
		)
		mustEmitWarning(t, emitter, warning)
		mustEmitResult(
			t,
			emitter,
			protocol.NewSuccessResult(
				protocol.StageDoctor,
				"succeeded",
				"doctor passed",
				map[string]any{},
			),
		)
	case contracttest.TerminalFailure:
		failure := mustNewErrorEvent(
			t,
			protocol.CodeUpdateStateAmbiguous,
			"doctor failed",
			map[string]any{"check": "runtime"},
		)
		mustEmitError(t, emitter, failure)
		mustEmitResult(
			t,
			emitter,
			protocol.NewFailureResult(failure, "failed", failure.Message, map[string]any{}),
		)
	case contracttest.TerminalCancelled:
		cancelled := mustNewErrorEvent(
			t,
			protocol.CodeOperationCancelled,
			"doctor cancelled",
			map[string]any{},
		)
		mustEmitError(t, emitter, cancelled)
		mustEmitResult(
			t,
			emitter,
			protocol.NewFailureResult(cancelled, "cancelled", cancelled.Message, map[string]any{}),
		)
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}

	return contracttest.Transcript{Stdout: stdout.Bytes()}
}

func mustNewWarningEvent(
	t *testing.T,
	code protocol.Code,
	message string,
	details map[string]any,
) protocol.WarningEvent {
	t.Helper()
	event, err := protocol.NewWarningEvent(code, protocol.StageDoctor, message, details)
	if err != nil {
		t.Fatalf("NewWarningEvent() error = %v", err)
	}
	return event
}

func mustNewErrorEvent(
	t *testing.T,
	code protocol.Code,
	message string,
	details map[string]any,
) protocol.ErrorEvent {
	t.Helper()
	event, err := protocol.NewErrorEvent(code, protocol.StageDoctor, message, details)
	if err != nil {
		t.Fatalf("NewErrorEvent() error = %v", err)
	}
	return event
}

func mustEmitWarning(t *testing.T, emitter *protocol.Emitter, event protocol.WarningEvent) {
	t.Helper()
	if err := emitter.EmitWarning(event); err != nil {
		t.Fatalf("EmitWarning() error = %v", err)
	}
}

func mustEmitError(t *testing.T, emitter *protocol.Emitter, event protocol.ErrorEvent) {
	t.Helper()
	if err := emitter.EmitError(event); err != nil {
		t.Fatalf("EmitError() error = %v", err)
	}
}

func mustEmitResult(t *testing.T, emitter *protocol.Emitter, event protocol.ResultEvent) {
	t.Helper()
	if err := emitter.EmitResult(event); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}
}
