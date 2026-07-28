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
		mustEmitWarning(t, emitter, protocol.WarningEvent{
			Code:        "OPTIONAL_CHECK_SKIPPED",
			Stage:       protocol.StageDoctor,
			Message:     "optional check skipped",
			Retryable:   false,
			Remediation: []string{"review-log"},
			Details:     map[string]any{"check": "optional"},
		})
		mustEmitResult(t, emitter, protocol.ResultEvent{
			Success:     true,
			Code:        "OK",
			Stage:       protocol.StageDoctor,
			Status:      "succeeded",
			Message:     "doctor passed",
			Retryable:   false,
			Remediation: []string{},
			Details:     map[string]any{},
		})
	case contracttest.TerminalFailure:
		failure := protocol.ErrorEvent{
			Code:        "DOCTOR_FAILED",
			Stage:       protocol.StageDoctor,
			Message:     "doctor failed",
			Retryable:   true,
			Remediation: []string{"retry"},
			Details:     map[string]any{"check": "runtime"},
		}
		mustEmitError(t, emitter, failure)
		mustEmitResult(t, emitter, protocol.ResultEvent{
			Success:     false,
			Code:        failure.Code,
			Stage:       failure.Stage,
			Status:      "failed",
			Message:     failure.Message,
			Retryable:   failure.Retryable,
			Remediation: failure.Remediation,
			Details:     map[string]any{},
		})
	case contracttest.TerminalCancelled:
		cancelled := protocol.ErrorEvent{
			Code:        string(protocol.CodeOperationCancelled),
			Stage:       protocol.StageDoctor,
			Message:     "doctor cancelled",
			Retryable:   false,
			Remediation: []string{"retry"},
			Details:     map[string]any{},
		}
		mustEmitError(t, emitter, cancelled)
		mustEmitResult(t, emitter, protocol.ResultEvent{
			Success:     false,
			Code:        cancelled.Code,
			Stage:       cancelled.Stage,
			Status:      "cancelled",
			Message:     cancelled.Message,
			Retryable:   cancelled.Retryable,
			Remediation: cancelled.Remediation,
			Details:     map[string]any{},
		})
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}

	return contracttest.Transcript{Stdout: stdout.Bytes()}
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
