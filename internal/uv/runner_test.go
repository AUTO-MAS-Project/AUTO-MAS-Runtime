package uv

import (
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
		os.Getenv("FAKE_UV_LARGE") == "" {
		return
	}
	if path := os.Getenv("FAKE_UV_STARTED"); path != "" {
		if err := os.WriteFile(path, []byte("started"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if path := os.Getenv("FAKE_UV_RECORD"); path != "" {
		lines := make([]string, 0, len(os.Environ())+len(os.Args))
		for index, argument := range os.Args {
			lines = append(lines, fmt.Sprintf("arg%d=%s", index, argument))
		}
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
		} {
			lines = append(lines, key+"="+os.Getenv(key))
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
