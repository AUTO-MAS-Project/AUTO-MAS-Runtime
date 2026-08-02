package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestExecute_NDJSONSessionErrorTupleMatchesResult(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "doctor")
	events := parseNDJSON(t, result.stdout)
	var errorEvent, resultEvent parsedEvent
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if errorEvent.raw == "" || resultEvent.raw == "" {
		t.Fatalf("missing error or result: error=%q result=%q", errorEvent.raw, resultEvent.raw)
	}
	for _, field := range []string{"code", "stage", "retryable"} {
		if errorEvent.object[field] != resultEvent.object[field] {
			t.Errorf(
				"field %q error=%v result=%v, want equal",
				field,
				errorEvent.object[field],
				resultEvent.object[field],
			)
		}
	}
	if !equalStringSlices(errorEvent.object["remediation"], resultEvent.object["remediation"]) {
		t.Errorf(
			"remediation error=%v result=%v, want equal",
			errorEvent.object["remediation"],
			resultEvent.object["remediation"],
		)
	}
}

func TestExecute_ExitCodeFromResultCode(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "cleanup")
	if result.exitCode != protocol.ExitCodeInvalidArgument {
		t.Fatalf("exit code = %d, want %d", result.exitCode, protocol.ExitCodeInvalidArgument)
	}
	events := parseNDJSON(t, result.stdout)
	last := events[len(events)-1]
	if got := eventString(last, "code"); got != string(protocol.CodeUnsupportedMode) {
		t.Errorf("result code = %q, want UNSUPPORTED_MODE", got)
	}
}

func TestExecute_UnimplementedCommandStableError(t *testing.T) {
	t.Parallel()
	commands := []string{
		"bootstrap",
		"workspace check",
		"workspace sync",
		"environment check",
		"environment ensure",
		"environment repair",
		"dependencies check",
		"dependencies sync",
		"dependencies rebuild",
		"backend supervise",
		"repair",
	}
	for _, command := range commands {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--output", "ndjson"}, strings.Fields(command)...)
			result := runCLI(t, context.Background(), args...)
			if result.exitCode != 2 {
				t.Fatalf("exit code = %d, want 2", result.exitCode)
			}
			events := parseNDJSON(t, result.stdout)
			var errorEvent, resultEvent parsedEvent
			for _, event := range events {
				switch eventType(event) {
				case string(protocol.TypeError):
					errorEvent = event
				case string(protocol.TypeResult):
					resultEvent = event
				}
			}
			if got := eventString(errorEvent, "code"); got != string(protocol.CodeUnsupportedMode) {
				t.Errorf("error code = %q, want UNSUPPORTED_MODE", got)
			}
			if got := eventString(errorEvent, "message"); got != "命令尚未实现" {
				t.Errorf("error message = %q, want 命令尚未实现", got)
			}
			if got := eventString(resultEvent, "code"); got != string(protocol.CodeUnsupportedMode) {
				t.Errorf("result code = %q, want UNSUPPORTED_MODE", got)
			}
			if success, ok := resultEvent.object["success"].(bool); !ok || success {
				t.Errorf("result success = %v, want false", resultEvent.object["success"])
			}
		})
	}
}

func equalStringSlices(left, right any) bool {
	leftValues, leftOK := left.([]any)
	rightValues, rightOK := right.([]any)
	if !leftOK || !rightOK || len(leftValues) != len(rightValues) {
		return false
	}
	for index := range leftValues {
		if leftValues[index] != rightValues[index] {
			return false
		}
	}
	return true
}
