package uv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func telemetryEnvironmentKeysForTest() []string {
	return []string{
		"AUTO_MAS_TELEMETRY",
		"AUTO_MAS_SENTRY_DSN",
		"AUTO_MAS_SENTRY_ENVIRONMENT",
		"AUTO_MAS_SENTRY_RELEASE",
	}
}

// TestRunner_WorkingDirDefaultsToProjectDir 覆盖一次性 uv 命令：所有既有调用方
// 都不传 WorkingDir，它们的 cwd 必须逐字节保持为 ProjectDir（C6 只改 managed 后端）。
func TestRunner_WorkingDirDefaultsToProjectDir(t *testing.T) {
	tests := []struct {
		name       string
		workingDir func(t *testing.T) string
		want       func(t *testing.T, runner *UVRunner, workingDir string) string
	}{
		{
			name:       "empty falls back to project dir",
			workingDir: func(*testing.T) string { return "" },
			want: func(_ *testing.T, runner *UVRunner, _ string) string {
				return filepath.Clean(runner.ProjectDir)
			},
		},
		{
			name:       "explicit value wins",
			workingDir: func(t *testing.T) string { return t.TempDir() },
			want: func(_ *testing.T, _ *UVRunner, workingDir string) string {
				return filepath.Clean(workingDir)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newTestRunner(t)
			workingDir := test.workingDir(t)
			recordPath := filepath.Join(t.TempDir(), "run-workingdir-record.txt")
			if _, err := runner.Run(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
				Stage:       protocol.StageDependenciesSync,
				WorkingDir:  workingDir,
				Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
			}); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			record := readTestRecord(t, recordPath)
			if got, want := record["cwd"], test.want(t, runner, workingDir); got != want {
				t.Fatalf("child cwd = %q, want %q", got, want)
			}
		})
	}
}

func TestRunner_ScrubsUnmanagedUVEnvironment(t *testing.T) {
	t.Setenv("UV_INSECURE_HOST", "unsafe.example")
	t.Setenv("uv_no_sources", "1")
	t.Setenv("UV_DEFAULT_INDEX", "https://unsafe.example/simple")
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: map[string]string{
		"UV_INSECURE_HOST":            "override.example",
		"UV_NO_VERIFY_HASHES":         "1",
		uvOfflineEnv:                  "1",
		uvPythonInstallMirrorEnv:      "https://mirror.example/python",
		"FAKE_UV_ALLOWED_FOR_TESTING": "1",
	}})
	for _, key := range []string{"UV_INSECURE_HOST", "UV_NO_SOURCES", "UV_DEFAULT_INDEX", "UV_NO_VERIFY_HASHES"} {
		if containsEnvironmentKeyMap(environment, key) {
			t.Fatalf("environment contains %q, want scrubbed", key)
		}
	}
	for key, want := range map[string]string{
		uvOfflineEnv:                  "1",
		uvPythonInstallMirrorEnv:      "https://mirror.example/python",
		"FAKE_UV_ALLOWED_FOR_TESTING": "1",
	} {
		if got := environment[key]; got != want {
			t.Fatalf("environment[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestRunner_ScrubsTelemetryEnvironment(t *testing.T) {
	for _, key := range telemetryEnvironmentKeysForTest() {
		t.Setenv(key, "host-value")
	}
	t.Setenv("AUTO_MAS_TEST_HOST_PASSTHROUGH", "host-value")
	recordPath := filepath.Join(t.TempDir(), "telemetry-environment-record.txt")
	runner := newTestRunner(t)
	environment := map[string]string{
		"FAKE_UV_RECORD":                   recordPath,
		"AUTO_MAS_TEST_OPTION_PASSTHROUGH": "option-value",
		uvOfflineEnv:                       "1",
	}
	for _, key := range telemetryEnvironmentKeysForTest() {
		environment[strings.ToLower(key)] = "option-value"
	}
	result, err := runner.Run(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
		Stage:       protocol.StageUVCheck,
		Environment: environment,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v, want exit 0", result, err)
	}
	record := readTestRecord(t, recordPath)
	assertTelemetryEnvironmentAbsent(t, record)
	for key, want := range map[string]string{
		"AUTO_MAS_TEST_HOST_PASSTHROUGH":   "host-value",
		"AUTO_MAS_TEST_OPTION_PASSTHROUGH": "option-value",
		uvOfflineEnv:                       "1",
		uvManagedPythonEnv:                 "1",
		uvNoModifyPathEnv:                  "1",
		uvPythonInstallBinEnv:              "0",
		uvPythonInstallDirEnv:              runner.PythonInstallDir,
		uvProjectEnvironment:               runner.ProjectEnvDir,
		uvCacheDirEnv:                      runner.CacheDir,
	} {
		if got := record[key]; got != want {
			t.Errorf("environment[%q] = %q, want %q", key, got, want)
		}
	}
	if got := record["PATH"]; got == "" {
		t.Fatal("environment[PATH] is empty, want inherited PATH")
	}
}

func TestRunner_InjectsManagedEnvironment(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "fake-uv-record.txt")
	runner := newTestRunner(t)
	result, err := runner.Run(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, RunOptions{
		Stage: protocol.StageDependenciesSync,
		Environment: map[string]string{
			"FAKE_UV_RECORD":      recordPath,
			uvManagedPythonEnv:    "0",
			uvNoModifyPathEnv:     "0",
			uvPythonInstallBinEnv: "1",
			uvProjectEnvironment:  "unsafe-environment",
			"PATH":                "unsafe-path",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0", result.ExitCode)
	}
	record := readTestRecord(t, recordPath)
	for key, want := range map[string]string{
		uvManagedPythonEnv:    "1",
		uvNoModifyPathEnv:     "1",
		uvPythonInstallBinEnv: "0",
		uvColorEnv:            "never",
		uvNoProgressEnv:       "1",
		uvNoSystemConfigEnv:   "1",
		uvNoConfigEnv:         "1",
		uvPythonInstallDirEnv: runner.PythonInstallDir,
		uvProjectEnvironment:  runner.ProjectEnvDir,
		uvCacheDirEnv:         runner.CacheDir,
	} {
		if got := record[key]; got != want {
			t.Errorf("environment[%q] = %q, want %q", key, got, want)
		}
	}
	if got := record["PATH"]; got == "unsafe-path" || got == "" {
		t.Fatalf("environment[PATH] = %q, want inherited non-empty PATH", got)
	}
	if record["arg1"] == "" {
		t.Fatalf("recorded arguments = %#v, want runner arguments", record)
	}
}

func TestRunner_ForwardsLinesAndExitCode(t *testing.T) {
	runner := newTestRunner(t)
	var lines []string
	var linesMu sync.Mutex
	result, err := runner.Run(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, RunOptions{
		Stage: protocol.StageUVVerify,
		Environment: map[string]string{
			"FAKE_UV_STDOUT": "out-one\nout-two\n",
			"FAKE_UV_STDERR": "err-one\nerr-two\n",
			"FAKE_UV_EXIT":   "7",
		},
		Line: func(_ context.Context, stream, line string) error {
			linesMu.Lock()
			defer linesMu.Unlock()
			lines = append(lines, stream+":"+line)
			return nil
		},
	})
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Run() error = %T %v, want uv Error", err, err)
	}
	if operationErr.Code() != protocol.CodeUVExecFailed {
		t.Fatalf("Run() code = %q, want %q", operationErr.Code(), protocol.CodeUVExecFailed)
	}
	if result.ExitCode != 7 {
		t.Fatalf("Run() exit code = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "out-one") || !strings.Contains(result.Stderr, "err-two") {
		t.Fatalf("Run() output = %#v, want both streams", result)
	}
	if len(lines) != 4 {
		t.Fatalf("forwarded lines = %#v, want 4 lines", lines)
	}
}

func TestRunner_LineCallbackErrorStopsProcess(t *testing.T) {
	runner := newTestRunner(t)
	callbackErr := errors.New("callback failed")
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(t.Context(), []string{
			"-test.run=^TestFakeUVProcess$",
		}, RunOptions{
			Stage: protocol.StageUVCheck,
			Environment: map[string]string{
				"FAKE_UV_STDOUT": "one\n",
				"FAKE_UV_DELAY":  "10s",
			},
			Line: func(context.Context, string, string) error {
				return callbackErr
			},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, callbackErr) {
			t.Fatalf("Run() error = %v, want callback cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not stop after line callback failure")
	}
}

func TestRunner_Cancel(t *testing.T) {
	startedPath := filepath.Join(t.TempDir(), "fake-uv-started")
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, []string{
			"-test.run=^TestFakeUVProcess$",
		}, RunOptions{
			Stage: protocol.StageUVCheck,
			Environment: map[string]string{
				"FAKE_UV_STARTED": startedPath,
				"FAKE_UV_DELAY":   "10s",
			},
		})
		done <- err
	}()
	waitForTestFile(t, startedPath)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestRunner_OversizedLineDrainsPipe(t *testing.T) {
	runner := newTestRunner(t)
	result, err := runner.Run(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, RunOptions{
		Stage: protocol.StageUVCheck,
		Environment: map[string]string{
			"FAKE_UV_LARGE": "2097152",
		},
	})
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Run() error = %T %v, want uv Error", err, err)
	}
	if operationErr.Code() != protocol.CodeUVExecFailed {
		t.Fatalf("Run() code = %q, want %q", operationErr.Code(), protocol.CodeUVExecFailed)
	}
	if result.ExitCode == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero after overflow", result.ExitCode)
	}
}

func TestRunner_OversizedUnterminatedLineStopsProcess(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct {
		result UVResult
		err    error
	}, 1)
	go func() {
		result, err := runner.Run(ctx, []string{
			"-test.run=^TestFakeUVProcess$",
		}, RunOptions{
			Stage: protocol.StageUVCheck,
			Environment: map[string]string{
				"FAKE_UV_LARGE":      strconv.Itoa(2 * 1024 * 1024),
				"FAKE_UV_NO_NEWLINE": "1",
				"FAKE_UV_DELAY":      "10s",
			},
		})
		done <- struct {
			result UVResult
			err    error
		}{result: result, err: err}
	}()
	var result UVResult
	var err error
	select {
	case outcome := <-done:
		result, err = outcome.result, outcome.err
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Run() did not stop after an unterminated oversized line")
		}
		t.Fatal("Run() took too long after an unterminated oversized line")
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Run() error = %T %v, want uv Error", err, err)
	}
	if operationErr.Code() != protocol.CodeUVExecFailed {
		t.Fatalf("Run() code = %q, want %q", operationErr.Code(), protocol.CodeUVExecFailed)
	}
	if result.ExitCode == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero after overflow", result.ExitCode)
	}
}

func TestRunner_RejectsExcessiveStreamOutput(t *testing.T) {
	runner := newTestRunner(t)
	result, err := runner.Run(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, RunOptions{
		Stage: protocol.StageUVCheck,
		Environment: map[string]string{
			"FAKE_UV_LARGE": strconv.Itoa(17 * 1024 * 1024),
		},
	})
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Run() error = %T %v, want uv Error", err, err)
	}
	if operationErr.Code() != protocol.CodeUVExecFailed {
		t.Fatalf("Run() code = %q, want %q", operationErr.Code(), protocol.CodeUVExecFailed)
	}
	if len(result.Stdout) > maxUVOutputBytes {
		t.Fatalf("captured stdout bytes = %d, want <= %d", len(result.Stdout), maxUVOutputBytes)
	}
}

// TestRunner_CapturesFullOutputOfFastExitingProcess 锁定 H-1 的修复。
//
// 修复前 Run() 用 command.StdoutPipe()/StderrPipe() 取读端，而那两个读端归 exec
// 所有：command.Wait() 一返回就把它们关掉。读取 goroutine 与 Wait 是并发的，
// 于是「子进程写完大于管道缓冲区的数据后立刻退出」这一形状会让仍在进行或尚未被
// 调度的读取拿到 os.ErrClosed，被 recordFirstError 记成 streamErr，
// 最终把一次**成功**的 uv 运行报成 UV_EXEC_FAILED「uv 输出读取失败」，
// 并且丢掉已经写出的输出。
//
// 修复后读端由 Run() 自己持有（os.Pipe()），Wait 无法提前关闭它，
// EOF 只取决于子进程树是否还持有写端。断言因此是「成功且输出完整」。
// 这个竞态是概率性的，所以重复若干轮以提高命中率；单轮通过不足以证明无竞态。
func TestRunner_CapturesFullOutputOfFastExitingProcess(t *testing.T) {
	const (
		lines     = 2000
		lineBytes = 200
	)
	for round := 0; round < 20; round++ {
		runner := newTestRunner(t)
		result, err := runner.Run(t.Context(), []string{
			"-test.run=^TestFakeUVProcess$",
		}, RunOptions{
			Stage: protocol.StageUVCheck,
			Environment: map[string]string{
				"FAKE_UV_LINES":      strconv.Itoa(lines),
				"FAKE_UV_LINE_BYTES": strconv.Itoa(lineBytes),
			},
		})
		if err != nil {
			t.Fatalf("round %d: Run() error = %v, want success", round, err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("round %d: exit code = %d, want 0", round, result.ExitCode)
		}
		if got := strings.Count(result.Stdout, "\n"); got != lines {
			t.Fatalf("round %d: captured %d lines, want %d", round, got, lines)
		}
		if !strings.Contains(result.Stdout, uvCaptureTruncatedNotice) {
			continue
		}
		t.Fatalf("round %d: output unexpectedly truncated below the capture budget", round)
	}
}

// TestRunner_TruncatesOversizedCaptureWithoutFailing 锁定 M-1 的修复。
//
// maxUVOutputBytes 是诊断快照上限，不是流上限（流上限是 maxUVStreamBytes）。
// 修复前触到快照上限会 recordFirstError，于是「输出足够多但完全正常」的
// uv sync 会确定性地失败在 UV_EXEC_FAILED，且重试无效——依赖越多越必然。
// 修复后只截断并追加说明标记，运行本身仍然成功。
func TestRunner_TruncatesOversizedCaptureWithoutFailing(t *testing.T) {
	// 每行 100 KiB 远低于 1 MiB 行上限；64 行合计约 6.4 MiB，
	// 超过 4 MiB 快照上限但远低于 16 MiB 流上限。
	const (
		lines     = 64
		lineBytes = 100 * 1024
	)
	runner := newTestRunner(t)
	result, err := runner.Run(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, RunOptions{
		Stage: protocol.StageUVCheck,
		Environment: map[string]string{
			"FAKE_UV_LINES":      strconv.Itoa(lines),
			"FAKE_UV_LINE_BYTES": strconv.Itoa(lineBytes),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want success despite capture truncation", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if !strings.HasSuffix(result.Stdout, uvCaptureTruncatedNotice) {
		t.Fatalf("captured stdout does not end with the truncation notice; tail=%q", tailForTest(result.Stdout, 80))
	}
	// 含标记的总长仍不得超过快照上限，否则截断标记本身就成了新的越界来源。
	if len(result.Stdout) > maxUVOutputBytes {
		t.Fatalf("captured stdout bytes = %d, want <= %d", len(result.Stdout), maxUVOutputBytes)
	}
}

func tailForTest(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}

func TestNormalizeVersionOutput_AllowsOnlyOneLineEnding(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "LF", input: "uv 0.12.3\n", want: "uv 0.12.3"},
		{name: "CRLF", input: "uv 0.12.3\r\n", want: "uv 0.12.3"},
		{name: "no ending", input: "uv 0.12.3", want: "uv 0.12.3"},
		{name: "extra whitespace", input: "uv 0.12.3\n\n", want: "uv 0.12.3\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := normalizeVersionOutput(testCase.input); got != testCase.want {
				t.Fatalf("normalizeVersionOutput() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestUVVersionOutput_MatchesOfficialGrammar(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		expected string
		want     bool
	}{
		{name: "short", output: "uv 0.12.3\n", expected: "0.12.3", want: true},
		{
			name:     "official build metadata",
			output:   "uv 0.12.3 (507230998 2026-08-07 x86_64-pc-windows-msvc)\r\n",
			expected: "0.12.3",
			want:     true,
		},
		{name: "wrong version", output: "uv 0.12.4 (build)", expected: "0.12.3", want: false},
		{name: "unwrapped metadata", output: "uv 0.12.3 build", expected: "0.12.3", want: false},
		{name: "empty metadata", output: "uv 0.12.3 ()", expected: "0.12.3", want: false},
		{name: "extra line", output: "uv 0.12.3\nspoof", expected: "0.12.3", want: false},
		{name: "trailing space", output: "uv 0.12.3 ", expected: "0.12.3", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := uvVersionOutputMatches(testCase.output, testCase.expected); got != testCase.want {
				t.Fatalf("uvVersionOutputMatches(%q, %q) = %t, want %t", testCase.output, testCase.expected, got, testCase.want)
			}
		})
	}
}

func TestUVRunner_StartFailureIncludesStableDiagnostics(t *testing.T) {
	runner := newTestRunner(t)
	missing := filepath.Join(t.TempDir(), "missing-project")
	_, err := runner.Run(t.Context(), []string{"--version"}, RunOptions{
		Stage:         protocol.StageBackendSpawn,
		ProjectDir:    missing,
		ProjectEnvDir: filepath.Join(missing, ".venv"),
	})
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Run() error = %T %v, want uv Error", err, err)
	}
	details := operationErr.Details()
	if details["operation"] != "start" || details["projectDir"] != missing ||
		details["projectEnvDir"] != filepath.Join(missing, ".venv") {
		t.Fatalf("Run() details = %#v, want stable start diagnostics", details)
	}
	if _, ok := details["windowsError"]; !ok {
		t.Fatalf("Run() details = %#v, want windowsError", details)
	}
}

func newTestRunner(t *testing.T) *UVRunner {
	t.Helper()
	projectDir := t.TempDir()
	runner, err := NewRunner(RunnerConfig{
		Executable:       testExecutable(t),
		ProjectDir:       projectDir,
		PythonInstallDir: filepath.Join(projectDir, "python"),
		ProjectEnvDir:    filepath.Join(projectDir, "venv"),
		CacheDir:         filepath.Join(projectDir, "cache"),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	return runner
}

func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	return path
}

func readTestRecord(t *testing.T, path string) map[string]string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			result[key] = value
		}
	}
	return result
}

func assertTelemetryEnvironmentAbsent(t *testing.T, environment map[string]string) {
	t.Helper()
	for _, key := range telemetryEnvironmentKeysForTest() {
		if containsEnvironmentKeyMap(environment, key) {
			t.Errorf("child environment contains Runtime-only telemetry key %q", key)
		}
	}
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %q", path)
		case <-ticker.C:
		}
	}
}

// TestFakeUVProcess 是由 UVRunner 子进程调用的假 uv 行为入口。
func TestFakeUVProcess(t *testing.T) {
	if os.Getenv("FAKE_UV_RECORD") == "" && os.Getenv("FAKE_UV_STARTED") == "" &&
		os.Getenv("FAKE_UV_STDOUT") == "" && os.Getenv("FAKE_UV_STDERR") == "" &&
		os.Getenv("FAKE_UV_EXIT") == "" && os.Getenv("FAKE_UV_DELAY") == "" &&
		os.Getenv("FAKE_UV_LARGE") == "" && os.Getenv("FAKE_UV_LINES") == "" {
		return
	}
	if path := os.Getenv("FAKE_UV_STARTED"); path != "" {
		if err := os.WriteFile(path, []byte("started"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if path := os.Getenv("FAKE_UV_RECORD"); path != "" {
		lines := make([]string, 0, len(os.Environ())+len(os.Args)+16)
		for index, argument := range os.Args {
			lines = append(lines, fmt.Sprintf("arg%d=%s", index, argument))
		}
		// cwd 是 T13.1 断言子进程工作目录的唯一真实证据：只有真实子进程
		// 报告的 Getwd 才能证明 StartSpec.Dir 生效，父进程侧断言做不到。
		workingDirectory, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "cwd="+filepath.Clean(workingDirectory))
		lines = append(lines, os.Environ()...)
		for _, key := range []string{
			uvPythonInstallDirEnv,
			uvCacheDirEnv,
			uvProjectEnvironment,
			uvManagedPythonEnv,
			uvNoModifyPathEnv,
			uvPythonInstallBinEnv,
			uvColorEnv,
			uvNoProgressEnv,
			"PATH",
			autoMASUVExecutable,
			autoMASProtocol,
			autoMASVersion,
			autoMASCommit,
			autoMASSupervised,
		} {
			if value, ok := os.LookupEnv(key); ok {
				lines = append(lines, key+"="+value)
			}
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if value := os.Getenv("FAKE_UV_STDOUT"); value != "" {
		_, _ = fmt.Fprint(os.Stdout, value)
	}
	if value := os.Getenv("FAKE_UV_STDERR"); value != "" {
		_, _ = fmt.Fprint(os.Stderr, value)
	}
	if value := os.Getenv("FAKE_UV_LARGE"); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("FAKE_UV_NO_NEWLINE") != "" {
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", size))
		} else {
			_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", size))
		}
	}
	// FAKE_UV_LINES/FAKE_UV_LINE_BYTES 产生多条「每条都不超行上限」的输出。
	// FAKE_UV_LARGE 只能产生单行，一旦超过 1 MiB 行上限就走失败路径，
	// 无法用来构造「总量很大但每行合法」的成功场景。
	if value := os.Getenv("FAKE_UV_LINES"); value != "" {
		lines, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		lineBytes := 200
		if raw := os.Getenv("FAKE_UV_LINE_BYTES"); raw != "" {
			lineBytes, err = strconv.Atoi(raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		payload := strings.Repeat("y", lineBytes)
		writer := bufio.NewWriter(os.Stdout)
		for index := 0; index < lines; index++ {
			if _, err := fmt.Fprintf(writer, "%d %s\n", index, payload); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if value := os.Getenv("FAKE_UV_DELAY"); value != "" {
		delay, err := time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(delay)
		<-timer.C
	}
	if value := os.Getenv("FAKE_UV_EXIT"); value != "" {
		code, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			os.Exit(code)
		}
	}
	os.Exit(0)
}

// TestRunner_AllowsHTTPTimeoutOverride 锁定增补 2 C17 第 6 条：中继路径可以注入 UV_HTTP_TIMEOUT，
// 但宿主环境里的同名变量仍被剔除，避免用户配置漂进受控调用。
func TestRunner_AllowsHTTPTimeoutOverride(t *testing.T) {
	t.Setenv(uvHTTPTimeoutEnv, "5")
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: map[string]string{
		"uv_http_timeout": "900",
	}})
	if got := environment[uvHTTPTimeoutEnv]; got != "900" {
		t.Fatalf("environment[%q] = %q, want 900", uvHTTPTimeoutEnv, got)
	}
	if containsEnvironmentKeyMap(environment, "uv_http_timeout") && environment["uv_http_timeout"] != "" {
		t.Fatalf("lowercase key survived, want canonical %q only", uvHTTPTimeoutEnv)
	}
}

// TestRunner_StillStripsHostHTTPTimeout 锁定不传覆盖时宿主的 UV_HTTP_TIMEOUT 不进 uv。
func TestRunner_StillStripsHostHTTPTimeout(t *testing.T) {
	t.Setenv(uvHTTPTimeoutEnv, "5")
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: map[string]string{}})
	if containsEnvironmentKeyMap(environment, uvHTTPTimeoutEnv) {
		t.Fatalf("environment contains %q = %q, want scrubbed", uvHTTPTimeoutEnv, environment[uvHTTPTimeoutEnv])
	}
}

// TestRunner_InjectsNoConfig 锁定增补 2 C20：uv 子进程同时带 UV_NO_CONFIG=1 与 UV_NO_SYSTEM_CONFIG=1，
// 用户级 %APPDATA%\uv\uv.toml、项目及其父目录里的 uv.toml / [tool.uv] 都不再参与；宿主与调用方都改不掉。
func TestRunner_InjectsNoConfig(t *testing.T) {
	t.Setenv(uvNoConfigEnv, "0")
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: map[string]string{
		uvNoConfigEnv:                  "0",
		strings.ToLower(uvNoConfigEnv): "0",
	}})
	for key, want := range map[string]string{
		uvNoConfigEnv:       "1",
		uvNoSystemConfigEnv: "1",
	} {
		if got := environment[key]; got != want {
			t.Fatalf("environment[%q] = %q, want %q", key, got, want)
		}
	}
	if got := environment[strings.ToLower(uvNoConfigEnv)]; got != "" {
		t.Fatalf("lowercase %q survived with %q", uvNoConfigEnv, got)
	}
}

// TestRunner_ScrubsHostPythonEnvironment 锁定增补 2 C20 的宿主隔离名单：会改变解释器或 uv 自身行为的
// PYTHON* / 虚拟环境 / 颜色 / Rust 调试变量不从宿主继承，只保留四个不改变「跑什么代码」的编码与缓冲变量；
// Runtime 自己经 RunOptions.Environment 显式注入的值不受名单影响。
func TestRunner_ScrubsHostPythonEnvironment(t *testing.T) {
	scrubbed := map[string]string{
		"PYTHONHOME":     `C:\bogus`,
		"PYTHONPATH":     `C:\bogus\lib`,
		"pythonsafepath": "1",
		"PYTHONWARNINGS": "error",
		"PYTHONOPTIMIZE": "2",
		"PYTHONINSPECT":  "1",
		"PYTHON_COLORS":  "1",
		"VIRTUAL_ENV":    `C:\bogus\venv`,
		"CONDA_PREFIX":   `C:\bogus\conda`,
		"FORCE_COLOR":    "1",
		"CLICOLOR_FORCE": "1",
		"NO_COLOR":       "1",
		"RUST_LOG":       "trace",
		"RUST_MIN_STACK": "1",
		"RUST_BACKTRACE": "full",
	}
	passthrough := map[string]string{
		"PYTHONIOENCODING":        "utf-8",
		"PYTHONUTF8":              "1",
		"PYTHONUNBUFFERED":        "1",
		"PYTHONDONTWRITEBYTECODE": "1",
	}
	for key, value := range scrubbed {
		t.Setenv(key, value)
	}
	for key, value := range passthrough {
		t.Setenv(key, value)
	}
	explicit := map[string]string{
		"PYTHONPATH":  `D:\explicit`,
		"VIRTUAL_ENV": `D:\explicit\venv`,
		"FORCE_COLOR": "explicit",
	}
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: explicit})
	for key := range scrubbed {
		if _, injected := explicit[key]; injected {
			continue
		}
		if containsEnvironmentKeyMap(environment, key) {
			t.Errorf("environment contains host %q = %q, want scrubbed", key, environment[key])
		}
	}
	for key, want := range passthrough {
		if got := environment[key]; got != want {
			t.Errorf("environment[%q] = %q, want host value %q", key, got, want)
		}
	}
	for key, want := range explicit {
		if got := environment[key]; got != want {
			t.Errorf("explicit %s = %q, want RunOptions value %q kept", key, got, want)
		}
	}
}

// TestRunner_LoopbackNeverProxied 锁定增补 2 C20：宿主代理变量原样继承，但 NO_PROXY 必含 127.0.0.1 与
// localhost——uv（reqwest）会把回环中继的请求也送进 HTTP(S)_PROXY / ALL_PROXY，且 NO_PROXY=localhost
// 并不豁免 127.0.0.1（uv 0.12.3 实测）。
func TestRunner_LoopbackNeverProxied(t *testing.T) {
	testCases := []struct {
		name     string
		hostKey  string
		hostVal  string
		override map[string]string
		want     string
	}{
		{name: "absent", want: "127.0.0.1,localhost"},
		{name: "host lowercase appended", hostKey: "no_proxy", hostVal: "intranet.example", want: "intranet.example,127.0.0.1,localhost"},
		{name: "already present keeps order", hostKey: "NO_PROXY", hostVal: " localhost , 127.0.0.1 ,,", want: "localhost,127.0.0.1"},
		{name: "case-insensitive dedupe", hostKey: "NO_PROXY", hostVal: "LOCALHOST", want: "LOCALHOST,127.0.0.1"},
		{name: "wildcard untouched", hostKey: "NO_PROXY", hostVal: "*", want: "*"},
		{name: "option override wins over host", hostKey: "NO_PROXY", hostVal: "host.example", override: map[string]string{"no_proxy": "option.example"}, want: "option.example,127.0.0.1,localhost"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
			t.Setenv("http_proxy", "http://proxy.example:8080")
			// 宿主可能带任意大小写的 NO_PROXY（Linux 上大小写是两个变量），逐个清掉再布置用例。
			for _, entry := range os.Environ() {
				if key, _, found := strings.Cut(entry, "="); found && strings.EqualFold(key, "NO_PROXY") {
					t.Setenv(key, "")
					os.Unsetenv(key)
				}
			}
			if testCase.hostKey != "" {
				t.Setenv(testCase.hostKey, testCase.hostVal)
			}
			runner := newTestRunner(t)
			environment := runner.EnvironmentForTesting(RunOptions{Environment: testCase.override})
			if got := environment["NO_PROXY"]; got != testCase.want {
				t.Fatalf("environment[NO_PROXY] = %q, want %q (full: %v)", got, testCase.want, environment)
			}
			count := 0
			for key := range environment {
				if strings.EqualFold(key, "NO_PROXY") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("NO_PROXY appears %d times, want exactly one canonical key", count)
			}
			for key, want := range map[string]string{"HTTPS_PROXY": "http://proxy.example:8080", "http_proxy": "http://proxy.example:8080"} {
				if got := environment[key]; got != want {
					t.Fatalf("environment[%q] = %q, want host proxy kept", key, got)
				}
			}
		})
	}
}
