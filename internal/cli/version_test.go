package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

func TestVersionCommand_HumanOutput(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "version")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	if !strings.Contains(result.stdout, "HELLO") {
		t.Errorf("stdout = %q, want human HELLO line", result.stdout)
	}
	if !strings.Contains(result.stdout, "Runtime dev") {
		t.Errorf("stdout = %q, want runtime version line", result.stdout)
	}
	if !strings.Contains(result.stdout, "协议 1") {
		t.Errorf("stdout = %q, want protocol version line", result.stdout)
	}
	if !strings.Contains(result.stdout, "RESULT success=true") {
		t.Errorf("stdout = %q, want human success RESULT line", result.stdout)
	}
	if strings.Contains(result.stderr, "ERROR") {
		t.Errorf("stderr = %q, want no error", result.stderr)
	}
}

func TestVersionCommand_NDJSONDetails(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "version")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	events := parseNDJSON(t, result.stdout)
	last := events[len(events)-1]
	if eventType(last) != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", eventType(last))
	}
	if success, ok := last.object["success"].(bool); !ok || !success {
		t.Errorf("result success = %v, want true", last.object["success"])
	}
	details, ok := last.object["details"].(map[string]any)
	if !ok {
		t.Fatalf("result details = %#v, want object", last.object["details"])
	}
	if got := details["runtimeVersion"]; got != "dev" {
		t.Errorf("details.runtimeVersion = %v, want dev", got)
	}
	if got := details["protocolVersion"]; got != float64(protocol.Version) {
		t.Errorf("details.protocolVersion = %v, want %d", got, protocol.Version)
	}
	if got := details["commit"]; got != "" {
		t.Errorf("details.commit = %v, want empty", got)
	}
	if got := details["buildDate"]; got != "" {
		t.Errorf("details.buildDate = %v, want empty", got)
	}
	if got, ok := details["goVersion"].(string); !ok || !strings.HasPrefix(got, "go") {
		t.Errorf("details.goVersion = %v, want go prefix", details["goVersion"])
	}
}

func TestVersionCommand_InjectedValues(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "version"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
		WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{
				Version:   "v9.9.9",
				Protocol:  protocol.Version,
				Commit:    "0123456789abcdef0123456789abcdef01234567",
				BuildDate: "2026-08-02",
				GoVersion: "go1.26.5",
			}, nil
		}),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	events := parseNDJSON(t, stdout.String())
	if got := eventString(events[0], "runtimeVersion"); got != "v9.9.9" {
		t.Errorf("hello runtimeVersion = %q, want v9.9.9", got)
	}
	details := events[len(events)-1].object["details"].(map[string]any)
	if got := details["runtimeVersion"]; got != "v9.9.9" {
		t.Errorf("details.runtimeVersion = %v, want v9.9.9", got)
	}
	if got := details["commit"]; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("details.commit = %v, want injected commit", got)
	}
	if got := details["buildDate"]; got != "2026-08-02" {
		t.Errorf("details.buildDate = %v, want injected build date", got)
	}
	if got := details["goVersion"]; got != "go1.26.5" {
		t.Errorf("details.goVersion = %v, want injected go version", got)
	}
}

func TestVersionCommand_SourceErrorMapsToOutputWriteFailed(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "version"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{}, errors.New("version source failed")
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent, resultEvent parsedEvent
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeOutputWriteFailed) {
		t.Errorf("error code = %q, want OUTPUT_WRITE_FAILED", got)
	}
	if got := eventString(errorEvent, "message"); got != "无法获取版本信息" {
		t.Errorf("error message = %q, want 无法获取版本信息", got)
	}
	if got := eventString(resultEvent, "code"); got != string(protocol.CodeOutputWriteFailed) {
		t.Errorf("result code = %q, want OUTPUT_WRITE_FAILED", got)
	}
}

func TestVersionCommand_CancelledSourceMapsToOperationCancelled(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "version"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{}, context.Canceled
		}),
	)
	if code != 130 {
		t.Fatalf("exit code = %d, want 130", code)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent, resultEvent parsedEvent
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeOperationCancelled) {
		t.Errorf("error code = %q, want OPERATION_CANCELLED", got)
	}
	if got := eventString(resultEvent, "status"); got != "cancelled" {
		t.Errorf("result status = %q, want cancelled", got)
	}
}
