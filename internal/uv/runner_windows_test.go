//go:build windows

package uv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type consoleProbeResult struct {
	ConsoleWindow uintptr
	Args          []string
}

func TestRunner_NoConsoleWindow(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags uint32
	}{
		{name: "console", flags: windows.CREATE_NEW_CONSOLE},
		{name: "no-console", flags: windows.CREATE_NO_WINDOW},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, testExecutable(t), "-test.run=^TestRunnerConsoleHostProcess$", "-test.v")
			command.Env = append(os.Environ(), "FAKE_UV_CONSOLE_HOST="+test.name)
			command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: test.flags}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("console host error = %v, want nil; output:\n%s", err, output)
			}
		})
	}
}

// TestRunnerConsoleHostProcess 固定父进程控制台条件，避免测试环境默认无控制台而漏检。
func TestRunnerConsoleHostProcess(t *testing.T) {
	mode := os.Getenv("FAKE_UV_CONSOLE_HOST")
	if mode == "" {
		return
	}
	window := consoleWindowForTest(t)
	if (window != 0) != (mode == "console") {
		t.Fatalf("host console window = %#x in mode %q, want console present iff console mode", window, mode)
	}
	for _, test := range []struct {
		name     string
		exitCode int
	}{
		{name: "success"},
		{name: "nonzero exit", exitCode: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newTestRunner(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestRunnerConsoleProbeProcess$", "--", "含 空格", `a"b`}
			result, err := runner.Run(ctx, args, RunOptions{
				Stage: protocol.StageUVCheck,
				Environment: map[string]string{
					"FAKE_UV_CONSOLE_PROBE": strconv.Itoa(test.exitCode),
				},
			})
			if result.ExitCode != test.exitCode {
				t.Fatalf("Run() exit code = %d, want %d; error = %v", result.ExitCode, test.exitCode, err)
			}
			if test.exitCode == 0 && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if test.exitCode != 0 {
				var uvErr *Error
				if !errors.As(err, &uvErr) || uvErr.Code() != protocol.CodeUVExecFailed {
					t.Fatalf("Run() error = %v, want UV_EXEC_FAILED", err)
				}
			}
			var probe consoleProbeResult
			if err := json.Unmarshal([]byte(result.Stdout), &probe); err != nil {
				t.Fatalf("decode stdout %q: %v", result.Stdout, err)
			}
			if probe.ConsoleWindow != 0 {
				t.Errorf("child console window = %#x, want 0", probe.ConsoleWindow)
			}
			if !slices.Equal(probe.Args, args) {
				t.Errorf("child args = %q, want %q", probe.Args, args)
			}
			if result.Stderr != "probe stderr\n" {
				t.Errorf("stderr = %q, want %q", result.Stderr, "probe stderr\n")
			}
		})
	}
}

// TestRunnerConsoleProbeProcess 在真实子进程内探测控制台，避免只断言启动标志。
func TestRunnerConsoleProbeProcess(t *testing.T) {
	value := os.Getenv("FAKE_UV_CONSOLE_PROBE")
	if value == "" {
		return
	}
	exitCode, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(consoleProbeResult{ConsoleWindow: consoleWindowForTest(t), Args: os.Args[1:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, string(output)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stderr, "probe stderr"); err != nil {
		t.Fatal(err)
	}
	os.Exit(exitCode)
}

func consoleWindowForTest(t *testing.T) uintptr {
	t.Helper()
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	if err := procedure.Find(); err != nil {
		t.Fatal(err)
	}
	// 返回 0 表示没有关联控制台，GetConsoleWindow 不定义 GetLastError 语义。
	window, _, _ := procedure.Call()
	return window
}
