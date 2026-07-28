package protocol_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestHumanRendererGolden(t *testing.T) {
	stdout, stderr, emitter := newHumanEmitter(t, "v1.0.0", "bootstrap", []string{"stdin.cancel", "state.v1"})
	current, total, percent := int64(18), int64(42), 42.86
	if err := emitter.EmitProgress(protocol.ProgressEvent{
		Stage: protocol.StageDependenciesSync, Status: protocol.ProgressRunning,
		Current: &current, Total: &total, Percent: &percent,
		Message: "正在安装 Python 依赖",
	}); err != nil {
		t.Fatalf("EmitProgress() error = %v", err)
	}
	const stateDetailsMarker = "state-details-must-not-appear"
	if err := emitter.EmitState(protocol.StateEvent{
		Stage: protocol.StageDependenciesSync, Status: protocol.StateSyncingEnvironment,
		Message: "正在精确同步 Python 环境",
		Details: map[string]any{"marker": stateDetailsMarker},
	}); err != nil {
		t.Fatalf("EmitState() error = %v", err)
	}
	if err := emitter.EmitLog(protocol.LogEvent{Source: "backend", Stream: "stderr", Message: "Traceback\nsecond line"}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	if err := emitter.EmitWarning(protocol.WarningEvent{
		Code: "BACKEND_FORCE_TERMINATED", Stage: protocol.StageBackendShutdown,
		Message: "后端已被强制终止", Retryable: false, Remediation: []string{"open-log"},
		Details: map[string]any{"mustNotAppear": "warning details"},
	}); err != nil {
		t.Fatalf("EmitWarning() error = %v", err)
	}
	if err := emitter.EmitError(protocol.ErrorEvent{
		Code: "DEPENDENCY_SYNC_FAILED", Stage: protocol.StageDependenciesSync,
		Message: "Python 依赖安装失败", Retryable: true, Remediation: []string{"retry-sync", "open-log"},
		Details: map[string]any{"mustNotAppear": "error details"},
	}); err != nil {
		t.Fatalf("EmitError() error = %v", err)
	}
	if err := emitter.EmitResult(protocol.ResultEvent{
		Success: false, Code: "DEPENDENCY_SYNC_FAILED", Stage: protocol.StageDependenciesSync,
		Status: "environment_broken", Message: "Python 依赖同步失败", Retryable: true,
		Remediation: []string{"retry-sync", "open-log"}, Details: map[string]any{"mustNotAppear": "result details"},
	}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}

	wantStdout := "HELLO runtime=v1.0.0 command=bootstrap capabilities=stdin.cancel,state.v1\n" +
		"PROGRESS [dependencies.sync] running current=18 total=42 percent=42.86% — 正在安装 Python 依赖\n" +
		"STATE [dependencies.sync] syncing_environment — 正在精确同步 Python 环境\n"
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout = %q, want %q", got, wantStdout)
	}
	wantStderr := "LOG [backend:stderr] — Traceback\n" +
		"LOG [backend:stderr] | second line\n" +
		"WARNING [backend.shutdown] BACKEND_FORCE_TERMINATED retryable=false remediation=open-log — 后端已被强制终止\n" +
		"ERROR [dependencies.sync] DEPENDENCY_SYNC_FAILED retryable=true remediation=retry-sync,open-log — Python 依赖安装失败\n" +
		"RESULT success=false code=DEPENDENCY_SYNC_FAILED stage=dependencies.sync status=environment_broken retryable=true remediation=retry-sync,open-log — Python 依赖同步失败\n"
	if got := stderr.String(); got != wantStderr {
		t.Errorf("stderr = %q, want %q", got, wantStderr)
	}
	if strings.Contains(stdout.String()+stderr.String(), stateDetailsMarker) {
		t.Errorf("human output contains state details marker %q", stateDetailsMarker)
	}
}

func TestHumanRendererRoutesLogsAndResults(t *testing.T) {
	stdout, stderr, emitter := newHumanEmitter(t, "v1.0.0", "doctor", nil)
	stdout.Reset()
	if err := emitter.EmitLog(protocol.LogEvent{Source: "backend", Stream: "stdout", Message: "out"}); err != nil {
		t.Fatalf("EmitLog(stdout) error = %v", err)
	}
	if err := emitter.EmitLog(protocol.LogEvent{Source: "backend", Stream: "stderr", Message: "err"}); err != nil {
		t.Fatalf("EmitLog(stderr) error = %v", err)
	}
	if err := emitter.EmitLog(protocol.LogEvent{Source: "backend", Stream: "unknown", Message: "unknown"}); err != nil {
		t.Fatalf("EmitLog(unknown) error = %v", err)
	}
	if err := emitter.EmitResult(protocol.ResultEvent{Success: true, Code: "OK", Stage: protocol.StageDoctor, Status: "ready", Message: "success", Retryable: true}); err != nil {
		t.Fatalf("EmitResult(success) error = %v", err)
	}
	if err := emitter.EmitResult(protocol.ResultEvent{Success: false, Code: "FAILED", Stage: protocol.StageDoctor, Status: "failed", Message: "failure"}); err != nil {
		t.Fatalf("EmitResult(failure) error = %v", err)
	}
	wantStdout := "LOG [backend:stdout] — out\n" +
		"RESULT success=true code=OK stage=doctor status=ready retryable=true remediation=- — success\n"
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout = %q, want %q", got, wantStdout)
	}
	wantStderr := "LOG [backend:stderr] — err\n" +
		"LOG [backend:unknown] — unknown\n" +
		"RESULT success=false code=FAILED stage=doctor status=failed retryable=false remediation=- — failure\n"
	if got := stderr.String(); got != wantStderr {
		t.Errorf("stderr = %q, want %q", got, wantStderr)
	}
}

func TestHumanRendererProgressOptions(t *testing.T) {
	current, total, percent := int64(18), int64(42), 42.86
	tests := []struct {
		name  string
		event protocol.ProgressEvent
		want  string
	}{
		{"none", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Message: "checking"}, "PROGRESS [doctor] running — checking\n"},
		{"current", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Message: "checking"}, "PROGRESS [doctor] running current=18 — checking\n"},
		{"total", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Total: &total, Message: "checking"}, "PROGRESS [doctor] running total=42 — checking\n"},
		{"percent", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Percent: &percent, Message: "checking"}, "PROGRESS [doctor] running percent=42.86% — checking\n"},
		{"current and total", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Total: &total, Message: "checking"}, "PROGRESS [doctor] running current=18 total=42 — checking\n"},
		{"current and percent", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Percent: &percent, Message: "checking"}, "PROGRESS [doctor] running current=18 percent=42.86% — checking\n"},
		{"total and percent", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Total: &total, Percent: &percent, Message: "checking"}, "PROGRESS [doctor] running total=42 percent=42.86% — checking\n"},
		{"all", protocol.ProgressEvent{Stage: protocol.StageDoctor, Status: protocol.ProgressRunning, Current: &current, Total: &total, Percent: &percent, Message: "checking"}, "PROGRESS [doctor] running current=18 total=42 percent=42.86% — checking\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, _, emitter := newHumanEmitter(t, "v1.0.0", "doctor", nil)
			stdout.Reset()
			if err := emitter.EmitProgress(test.event); err != nil {
				t.Fatalf("EmitProgress() error = %v", err)
			}
			if got := stdout.String(); got != test.want {
				t.Errorf("stdout = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHumanRendererContractMatrix(t *testing.T) {
	t.Run("empty values unknown fields and details", func(t *testing.T) {
		stdout, stderr, emitter := newHumanEmitter(t, "", "", nil)
		if err := emitter.EmitProgress(protocol.ProgressEvent{Stage: "unknown\rstage", Status: "unknown\nstatus", Message: "progress", Percent: float64Pointer(1e-9)}); err != nil {
			t.Fatalf("EmitProgress() error = %v", err)
		}
		if err := emitter.EmitWarning(protocol.WarningEvent{Stage: "unknown-stage", Code: "unknown-code", Message: "warning", Retryable: true, Details: map[string]any{"hidden": "value"}}); err != nil {
			t.Fatalf("EmitWarning() error = %v", err)
		}
		if err := emitter.EmitError(protocol.ErrorEvent{Stage: "unknown-stage", Code: "unknown-code", Message: "error", Retryable: false, Details: map[string]any{"hidden": "value"}}); err != nil {
			t.Fatalf("EmitError() error = %v", err)
		}
		if err := emitter.EmitResult(protocol.ResultEvent{Success: true, Code: "unknown-code", Stage: "unknown-stage", Status: "unknown-status", Message: "success", Retryable: false, Details: map[string]any{"hidden": "value"}}); err != nil {
			t.Fatalf("EmitResult(success) error = %v", err)
		}
		if err := emitter.EmitResult(protocol.ResultEvent{Success: false, Code: "unknown-code", Stage: "unknown-stage", Status: "unknown-status", Message: "failure", Retryable: true, Details: map[string]any{"hidden": "value"}}); err != nil {
			t.Fatalf("EmitResult(failure) error = %v", err)
		}
		wantStdout := "HELLO runtime=- command=- capabilities=-\n" +
			"PROGRESS [unknown\\rstage] unknown\\nstatus percent=0.000000001% — progress\n" +
			"RESULT success=true code=unknown-code stage=unknown-stage status=unknown-status retryable=false remediation=- — success\n"
		if got := stdout.String(); got != wantStdout {
			t.Errorf("stdout = %q, want %q", got, wantStdout)
		}
		wantStderr := "WARNING [unknown-stage] unknown-code retryable=true remediation=- — warning\n" +
			"ERROR [unknown-stage] unknown-code retryable=false remediation=- — error\n" +
			"RESULT success=false code=unknown-code stage=unknown-stage status=unknown-status retryable=true remediation=- — failure\n"
		if got := stderr.String(); got != wantStderr {
			t.Errorf("stderr = %q, want %q", got, wantStderr)
		}
		if strings.Contains(stdout.String()+stderr.String(), "hidden") {
			t.Error("details appeared in human output")
		}
	})
}

func TestHumanRendererNormalizesMessages(t *testing.T) {
	stdout, stderr, emitter := newHumanEmitter(t, "v\\ersion\r\nnext", "com\rmand\nnext", []string{"cap\\ability\r\nnext", string([]byte{'b', 'a', 'd', 0xff})})
	if err := emitter.EmitLog(protocol.LogEvent{
		Source: "so\\urce\r\nnext", Stream: "st\rream\nnext",
		Message: "first\r\nsecond\r\n\rthird\\slash\x1b\x00\u0085" + string([]byte{0xff}) + "\n",
	}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	if err := emitter.EmitError(protocol.ErrorEvent{
		Stage: "st\r\nage", Code: "co\rde\nnext", Message: "error",
		Remediation: []string{"re\rmedy\nnext"},
	}); err != nil {
		t.Fatalf("EmitError() error = %v", err)
	}
	if err := emitter.EmitState(protocol.StateEvent{Stage: "state", Status: "empty", Message: ""}); err != nil {
		t.Fatalf("EmitState() error = %v", err)
	}
	wantStdout := "HELLO runtime=v\\\\ersion\\r\\nnext command=com\\rmand\\nnext capabilities=cap\\\\ability\\r\\nnext,bad�\n"
	wantStdout += "STATE [state] empty — \n"
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout = %q, want %q", got, wantStdout)
	}
	wantStderr := "LOG [so\\\\urce\\r\\nnext:st\\rream\\nnext] — first\n" +
		"LOG [so\\\\urce\\r\\nnext:st\\rream\\nnext] | second\n" +
		"LOG [so\\\\urce\\r\\nnext:st\\rream\\nnext] | \n" +
		"LOG [so\\\\urce\\r\\nnext:st\\rream\\nnext] | third\\\\slash\\x1b\\x00\\x85�\n" +
		"LOG [so\\\\urce\\r\\nnext:st\\rream\\nnext] | \n" +
		"ERROR [st\\r\\nage] co\\rde\\nnext retryable=false remediation=re\\rmedy\\nnext — error\n"
	if got := stderr.String(); got != wantStderr {
		t.Errorf("stderr = %q, want %q", got, wantStderr)
	}
}

func TestHumanRendererEscapesScalarControls(t *testing.T) {
	stdout, stderr, emitter := newHumanEmitter(t, "\x1b\x00\u0085\u200b", "command", nil)
	if err := emitter.EmitResult(protocol.ResultEvent{
		Success: false, Code: "co\x1bde", Stage: "st\u2028age", Status: "status\u0085", Message: "message",
		Retryable: true, Remediation: []string{"re\x00medy", "next\u2028"},
	}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}
	if want := "HELLO runtime=\\x1b\\x00\\x85\u200b command=command capabilities=-\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	wantStderr := "RESULT success=false code=co\\x1bde stage=st\u2028age status=status\\x85 retryable=true remediation=re\\x00medy,next\u2028 — message\n"
	if got := stderr.String(); got != wantStderr {
		t.Errorf("stderr = %q, want %q", got, wantStderr)
	}
}

func TestHumanRendererNeverEmitsANSIControls(t *testing.T) {
	stdout, stderr, emitter := newHumanEmitter(t, "\x1b[31m", "\u009b31m", []string{"\x00", "\u0085"})
	if err := emitter.EmitLog(protocol.LogEvent{Source: "\x1b", Stream: "stdout", Message: "\x1b[2J\x00\u0085"}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	output := stdout.String() + stderr.String()
	for _, runeValue := range output {
		if runeValue != '\n' && unicode.IsControl(runeValue) {
			t.Errorf("output contains raw control rune U+%04X: %q", runeValue, output)
		}
	}
	if strings.Contains(output, "\x1b") {
		t.Errorf("output contains raw ESC: %q", output)
	}
}

func TestHumanRendererConstructorsRejectNilWriters(t *testing.T) {
	var valid bytes.Buffer
	var typedNil *bytes.Buffer
	tests := []struct {
		name   string
		stdout io.Writer
		stderr io.Writer
	}{
		{"nil stdout", nil, &valid},
		{"nil stderr", &valid, nil},
		{"typed nil stdout", typedNil, &valid},
		{"typed nil stderr", &valid, typedNil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := protocol.NewHumanRenderer(test.stdout, test.stderr); err == nil {
				t.Errorf("NewHumanRenderer(%T, %T) error = nil, want nil-writer error", test.stdout, test.stderr)
			}
		})
	}
}

func newHumanEmitter(t *testing.T, runtimeVersion, command string, capabilities []string) (*bytes.Buffer, *bytes.Buffer, *protocol.Emitter) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	renderer, err := protocol.NewHumanRenderer(stdout, stderr)
	if err != nil {
		t.Fatalf("NewHumanRenderer() error = %v", err)
	}
	output, err := protocol.NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	clock := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	emitter, err := output.NewEmitter(
		runtimeVersion, command, capabilities,
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return clock }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	return stdout, stderr, emitter
}

func float64Pointer(value float64) *float64 {
	return &value
}
