package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

// TestExecute_FrameworkSatisfiesContract 用 contracttest.Assert 证明
// 会话框架的失败终态满足完整 NDJSON 契约（hello 首发、sequence 递增、
// result 恰好一次且最后、error/result 四元组一致、容器字段非空）。
func TestExecute_FrameworkSatisfiesContract(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "repair")
	contracttest.Assert(
		t,
		"repair",
		contracttest.TerminalFailure,
		[]byte(result.stdout),
	)
}

func TestExecute_NDJSONSessionErrorTupleMatchesResult(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "repair")
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

// TestExecute_CancelledWrappedErrorMapsToExit130 证明即使业务错误声明了
// 非取消错误码，只要错误链包含 context.Canceled，顶层映射仍以取消优先
// （设计 §13 顺序），退出码 130 且 error/result code 均为 OPERATION_CANCELLED。
func TestExecute_CancelledWrappedErrorMapsToExit130(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{
				run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
					return doctor.Report{}, doctor.NewError(
						protocol.CodeGitRepoCleanupFailed,
						protocol.StageDoctor,
						"清理失败",
						map[string]any{},
						fmt.Errorf("cleanup: %w", context.Canceled),
					)
				},
			}, nil
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
	if got := eventString(resultEvent, "code"); got != string(protocol.CodeOperationCancelled) {
		t.Errorf("result code = %q, want OPERATION_CANCELLED", got)
	}
	if got := eventString(resultEvent, "status"); got != "cancelled" {
		t.Errorf("result status = %q, want cancelled", got)
	}
}

func TestExecute_ExitCodeFromResultCode(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "bootstrap")
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
