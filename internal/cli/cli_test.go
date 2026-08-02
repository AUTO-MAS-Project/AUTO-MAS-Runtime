package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// runResult 保存一次 Execute 的退出码与双通道输出。
type runResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runCLI 以独立临时 cwd 执行 CLI，返回退出码与输出。
func runCLI(t *testing.T, ctx context.Context, args ...string) runResult {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Execute(
		ctx,
		args,
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
	)
	return runResult{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}
}

// parsedEvent 是测试对 NDJSON 行的最小视图。
type parsedEvent struct {
	raw    string
	object map[string]any
}

// parseNDJSON 按物理行解析 stdout，任何非 JSON 或空行都会失败测试。
func parseNDJSON(t *testing.T, stdout string) []parsedEvent {
	t.Helper()
	if stdout == "" {
		t.Fatal("stdout is empty")
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout must end with LF: %q", stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	events := make([]parsedEvent, 0, len(lines))
	for index, line := range lines {
		if line == "" {
			t.Fatalf("physical line %d is empty", index+1)
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", index+1, err)
		}
		events = append(events, parsedEvent{raw: line, object: object})
	}
	return events
}

func eventType(event parsedEvent) string {
	value, _ := event.object["type"].(string)
	return value
}

func eventString(event parsedEvent, field string) string {
	value, _ := event.object[field].(string)
	return value
}

func TestExecute_NoArgsPrintsHelp(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background())
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	if !strings.Contains(result.stdout, "auto-mas-runtime") {
		t.Fatalf("stdout = %q, want usage header", result.stdout)
	}
}

func TestExecute_DefaultHumanOutput(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "repair")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if !strings.Contains(result.stdout, "HELLO") {
		t.Errorf("stdout = %q, want human HELLO line", result.stdout)
	}
	if !strings.Contains(result.stderr, "ERROR") {
		t.Errorf("stderr = %q, want human ERROR line", result.stderr)
	}
	if !strings.Contains(result.stderr, "RESULT") {
		t.Errorf("stderr = %q, want human failure RESULT line", result.stderr)
	}
}

func TestExecute_NDJSONSessionHelloFirstResultLast(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "repair")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	events := parseNDJSON(t, result.stdout)
	if len(events) < 3 {
		t.Fatalf("event count = %d, want hello+error+result", len(events))
	}
	if eventType(events[0]) != string(protocol.TypeHello) {
		t.Errorf("first event type = %q, want hello", eventType(events[0]))
	}
	if eventType(events[len(events)-1]) != string(protocol.TypeResult) {
		t.Errorf("last event type = %q, want result", eventType(events[len(events)-1]))
	}
	if got := eventString(events[0], "command"); got != "repair" {
		t.Errorf("hello command = %q, want repair", got)
	}
}

func TestExecute_ProtocolMatchRunsSession(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--protocol", "1", "--output", "ndjson", "repair")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	events := parseNDJSON(t, result.stdout)
	if eventType(events[0]) != string(protocol.TypeHello) {
		t.Errorf("first event type = %q, want hello", eventType(events[0]))
	}
}

func TestExecute_ParseFailureHasNoProtocolSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "unknown flag", args: []string{"--bogus", "doctor"}, wantCode: 2},
		{name: "invalid output", args: []string{"--output", "yaml", "doctor"}, wantCode: 2},
		{name: "invalid app root", args: []string{"--app-root", "bad\x00root", "doctor"}, wantCode: 2},
		{name: "invalid protocol value", args: []string{"--protocol", "abc", "doctor"}, wantCode: 2},
		{name: "protocol mismatch", args: []string{"--protocol", "2", "doctor"}, wantCode: 10},
		{name: "mirror missing equals", args: []string{"--mirror", "git", "doctor"}, wantCode: 2},
		{name: "mirror unknown kind", args: []string{"--mirror", "ftp=github", "doctor"}, wantCode: 2},
		{name: "mirror invalid key", args: []string{"--mirror", "git=Github!", "doctor"}, wantCode: 2},
		{name: "mirror duplicate kind", args: []string{"--mirror", "git=cnb", "--mirror", "git=github", "doctor"}, wantCode: 2},
		{name: "offline with mirror", args: []string{"--offline", "--mirror", "git=cnb", "doctor"}, wantCode: 2},
		{name: "offline with mirror only", args: []string{"--offline", "--mirror-only", "doctor"}, wantCode: 2},
		{name: "unknown command", args: []string{"mystery"}, wantCode: 2},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), test.args...)
			if result.exitCode != test.wantCode {
				t.Errorf("exit code = %d, want %d", result.exitCode, test.wantCode)
			}
			if result.stdout != "" {
				t.Errorf("stdout = %q, want empty (no protocol session)", result.stdout)
			}
			if result.stderr == "" {
				t.Error("stderr is empty, want diagnostic")
			}
		})
	}
}

func TestExecute_InvalidOutputExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "yaml", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_InvalidAppRootExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--app-root", "bad\x00root", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_InvalidProtocolValueExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--protocol", "abc", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_ProtocolMismatchExit10(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--protocol", "2", "doctor")
	if result.exitCode != 10 {
		t.Fatalf("exit code = %d, want 10", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
	if !strings.Contains(result.stderr, "protocol") {
		t.Errorf("stderr = %q, want protocol diagnostic", result.stderr)
	}
}

func TestExecute_OfflineAloneValid(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--offline", "--output", "ndjson", "repair")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (skeleton failure)", result.exitCode)
	}
	events := parseNDJSON(t, result.stdout)
	if eventType(events[0]) != string(protocol.TypeHello) {
		t.Errorf("first event type = %q, want hello", eventType(events[0]))
	}
}

func TestExecute_OfflineWithMirrorExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--offline", "--mirror", "git=cnb", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
}

func TestExecute_OfflineWithMirrorOnlyExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--offline", "--mirror-only", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
}

func TestExecute_MirrorInvalidSyntaxExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--mirror", "git", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_MirrorUnknownKindExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--mirror", "ftp=github", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_MirrorInvalidKeyExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--mirror", "git=Github!", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_MirrorDuplicateKindExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--mirror", "git=cnb", "--mirror", "git=github", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
}

func TestExecute_UnknownFlagExit2(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--bogus", "doctor")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
}

func TestExecute_ContextCancelledMapsToOperationCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runCLI(t, ctx, "--output", "ndjson", "repair")
	if result.exitCode != 130 {
		t.Fatalf("exit code = %d, want 130", result.exitCode)
	}
	events := parseNDJSON(t, result.stdout)
	if eventType(events[0]) != string(protocol.TypeHello) {
		t.Errorf("first event type = %q, want hello", eventType(events[0]))
	}
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
