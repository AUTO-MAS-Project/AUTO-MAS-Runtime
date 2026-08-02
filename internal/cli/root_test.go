package cli

import (
	"context"
	"strings"
	"testing"
)

func TestExecute_HelpListsArchitectureCommandTree(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--help")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	for _, want := range []string{
		"version",
		"doctor",
		"bootstrap",
		"workspace",
		"environment",
		"dependencies",
		"backend",
		"repair",
		"cleanup",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("help stdout does not contain %q: %s", want, result.stdout)
		}
	}
	if strings.Contains(result.stdout, "completion") {
		t.Errorf("help stdout contains completion command: %s", result.stdout)
	}
	if strings.Contains(result.stdout, "help [command]") {
		t.Errorf("help stdout contains default help command: %s", result.stdout)
	}
}

func TestExecute_SubcommandHelpShowsOwnFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command []string
		want    string
	}{
		{command: []string{"bootstrap", "--help"}, want: "--version"},
		{command: []string{"workspace", "sync", "--help"}, want: "--version"},
		{command: []string{"backend", "supervise", "--help"}, want: "supervise"},
	}
	for _, test := range tests {
		test := test
		t.Run(strings.Join(test.command, " "), func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), test.command...)
			if result.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0", result.exitCode)
			}
			if !strings.Contains(result.stdout, test.want) {
				t.Errorf("help stdout does not contain %q: %s", test.want, result.stdout)
			}
		})
	}
}

func TestExecute_NDJSONHelpGoesToStderr(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "--help")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
	if !strings.Contains(result.stderr, "version") {
		t.Errorf("stderr = %q, want help text", result.stderr)
	}
}

func TestExecute_HelpCommandHiddenFromTree(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--help")
	if strings.Contains(result.stdout, "\n  help") {
		t.Errorf("help output lists help command: %s", result.stdout)
	}
}

// TestExecute_RejectsExtraPositionalArgs 证明所有叶子命令拒绝多余位置参数：
// 走参数错误路径（stderr 诊断 + 退出码 2），不发射 hello/result。
func TestExecute_RejectsExtraPositionalArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "version", args: []string{"version", "extra"}},
		{name: "doctor", args: []string{"doctor", "extra"}},
		{name: "cleanup", args: []string{"cleanup", "extra"}},
		{name: "bootstrap", args: []string{"bootstrap", "extra"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), test.args...)
			if result.exitCode != 2 {
				t.Errorf("exit code = %d, want 2", result.exitCode)
			}
			if result.stdout != "" {
				t.Errorf("stdout = %q, want empty (no protocol session)", result.stdout)
			}
			if !strings.Contains(result.stderr, "auto-mas-runtime: ") {
				t.Errorf("stderr = %q, want auto-mas-runtime: prefix", result.stderr)
			}
		})
	}
}
