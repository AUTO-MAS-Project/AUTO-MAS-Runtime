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

// TestExecute_GroupUnknownSubcommandRejected 证明组命令带未知子命令
// 按参数错误处理：stderr 诊断 + exit 2，不输出帮助、不发射协议事件。
func TestExecute_GroupUnknownSubcommandRejected(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "workspace", "foo")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty (no help, no protocol session)", result.stdout)
	}
	if !strings.Contains(result.stderr, "auto-mas-runtime: ") {
		t.Errorf("stderr = %q, want auto-mas-runtime: prefix", result.stderr)
	}
}

// TestExecute_GroupUnknownSubcommandNDJSONRejected 证明 ndjson 模式下
// 组命令未知子命令同样走参数错误：exit 2 且 stdout 保持空（机器可解析）。
func TestExecute_GroupUnknownSubcommandNDJSONRejected(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "workspace", "foo")
	if result.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
}

// TestExecute_GroupBareInvocationStillHelp 证明组命令裸调用与 --help
// 维持 help + exit 0（设计 §3.4 不回退），四个组命令各覆盖两种子用例。
func TestExecute_GroupBareInvocationStillHelp(t *testing.T) {
	t.Parallel()
	for _, group := range []string{
		"workspace",
		"environment",
		"dependencies",
		"backend",
	} {
		group := group
		t.Run(group+" bare", func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), group)
			if result.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0", result.exitCode)
			}
			if !strings.Contains(result.stdout, group) {
				t.Errorf("stdout = %q, want help text for %q", result.stdout, group)
			}
		})
		t.Run(group+" help", func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), group, "--help")
			if result.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0", result.exitCode)
			}
			if !strings.Contains(result.stdout, group) {
				t.Errorf("stdout = %q, want help text for %q", result.stdout, group)
			}
		})
	}
}
