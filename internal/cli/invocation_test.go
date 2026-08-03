package cli

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// resolveErrKind 是 resolveInvocation 三种结果的稳定分类，
// 与设计 §6 表格的三行一一对应：成功 / 参数错误(2) / 协议不兼容(10)。
type resolveErrKind int

const (
	resolveOK resolveErrKind = iota
	resolveArgumentError
	resolveProtocolMismatch
)

func (k resolveErrKind) String() string {
	switch k {
	case resolveOK:
		return "ok"
	case resolveArgumentError:
		return "argument-error"
	default:
		return "protocol-mismatch"
	}
}

func classifyResolveErr(err error) resolveErrKind {
	switch {
	case err == nil:
		return resolveOK
	case errors.Is(err, errProtocolMismatch):
		return resolveProtocolMismatch
	default:
		return resolveArgumentError
	}
}

func newResolveTestRoot(t *testing.T) *deps {
	t.Helper()
	return &deps{
		options: options{cwd: t.TempDir(), clock: time.Now},
	}
}

// TestResolveInvocation 是命令分发判定的表驱动契约。
//
// 分发此前散在 Execute 里，由 T3.5 F5、T3.6 F1、T3.6 F3 三轮补丁叠成，
// 没有任何测试守住「预解析树与执行树命令集合一致」和各分支的优先级。
// 本表固定这些优先级：output/protocol 校验先于 --help；--help 先于
// 「组命令带多余位置参数」；根级未知命令由 Find 提前拒绝。
func TestResolveInvocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantTarget string
		wantMode   outputMode
		wantHelp   bool
		wantErr    resolveErrKind
	}{
		{
			name:       "bare root falls through to cobra help",
			args:       nil,
			wantTarget: "auto-mas-runtime",
			wantMode:   outputHuman,
		},
		{
			name:       "leaf command",
			args:       []string{"doctor"},
			wantTarget: "doctor",
			wantMode:   outputHuman,
		},
		{
			name:       "nested leaf command",
			args:       []string{"workspace", "sync"},
			wantTarget: "sync",
			wantMode:   outputHuman,
		},
		{
			name:       "default output is human",
			args:       []string{"version"},
			wantTarget: "version",
			wantMode:   outputHuman,
		},
		{
			name:       "explicit ndjson output",
			args:       []string{"--output", "ndjson", "doctor"},
			wantTarget: "doctor",
			wantMode:   outputNDJSON,
		},
		{
			name:       "root help flag",
			args:       []string{"--help"},
			wantTarget: "auto-mas-runtime",
			wantMode:   outputHuman,
			wantHelp:   true,
		},
		{
			name:       "leaf help flag",
			args:       []string{"doctor", "--help"},
			wantTarget: "doctor",
			wantMode:   outputHuman,
			wantHelp:   true,
		},
		{
			name:       "help command is reachable in the probe tree",
			args:       []string{"help", "doctor"},
			wantTarget: "help",
			wantMode:   outputHuman,
		},
		{
			name:       "group bare invocation",
			args:       []string{"workspace"},
			wantTarget: "workspace",
			wantMode:   outputHuman,
		},
		{
			name:       "group help flag beats extra positional args",
			args:       []string{"workspace", "foo", "--help"},
			wantTarget: "workspace",
			wantMode:   outputHuman,
			wantHelp:   true,
		},
		{
			name:    "group unknown subcommand is an argument error",
			args:    []string{"workspace", "foo"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "root unknown command is an argument error",
			args:    []string{"nope"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "unknown flag is an argument error",
			args:    []string{"doctor", "--nope"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "invalid output is an argument error",
			args:    []string{"--output", "xml", "doctor"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "invalid output beats help flag",
			args:    []string{"--output", "xml", "--help"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "non integer protocol is an argument error",
			args:    []string{"--protocol", "one", "doctor"},
			wantErr: resolveArgumentError,
		},
		{
			name:    "incompatible protocol is a protocol mismatch",
			args:    []string{"--protocol", "999", "doctor"},
			wantErr: resolveProtocolMismatch,
		},
		{
			name:    "incompatible protocol beats help flag",
			args:    []string{"--protocol", "999", "--help"},
			wantErr: resolveProtocolMismatch,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			call, err := resolveInvocation(newRoot(newResolveTestRoot(t)), test.args)
			if got := classifyResolveErr(err); got != test.wantErr {
				t.Fatalf("error kind = %v (err=%v), want %v", got, err, test.wantErr)
			}
			if test.wantErr != resolveOK {
				return
			}
			if call.target == nil {
				t.Fatal("target = nil, want resolved command")
			}
			if got := call.target.Name(); got != test.wantTarget {
				t.Errorf("target = %q, want %q", got, test.wantTarget)
			}
			if call.mode != test.wantMode {
				t.Errorf("mode = %q, want %q", call.mode, test.wantMode)
			}
			if call.help != test.wantHelp {
				t.Errorf("help = %t, want %t", call.help, test.wantHelp)
			}
		})
	}
}

// TestResolveInvocation_ProbeTreeMatchesExecutionTree 固定 F7 的核心不变量：
// 预解析树与正式执行树必须是同构的两棵独立树。两棵树的命令集合一旦分叉，
// 预解析就会对执行树里存在的命令报「未知命令」——T3.7 F1 的 help 子命令
// 正是这样死了两个里程碑。
func TestResolveInvocation_ProbeTreeMatchesExecutionTree(t *testing.T) {
	t.Parallel()
	d := newResolveTestRoot(t)
	probe := newRoot(d)
	execution := newRoot(d)
	if probe == execution {
		t.Fatal("newRoot returned the same instance twice; trees must be independent")
	}
	got := commandNames(probe)
	want := commandNames(execution)
	if len(got) != len(want) {
		t.Fatalf("probe tree has %d commands, execution tree has %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(got) == 0 {
		t.Fatal("command tree is empty; the comparison proves nothing")
	}
}

// TestNewRoot_HelpFlagUsageIsChinese 证明 Cobra 自动生成的 -h/--help 用法
// 文案已本地化：帮助输出不再出现 "help for auto-mas-runtime" 这类与全中文
// Short/Long 混排的英文默认值（T3.8 F13g）。
func TestNewRoot_HelpFlagUsageIsChinese(t *testing.T) {
	t.Parallel()
	root := newRoot(newResolveTestRoot(t))
	for _, cmd := range []*cobra.Command{root, mustFindCommand(t, root, "doctor")} {
		flag := cmd.Flags().Lookup("help")
		if flag == nil {
			t.Fatalf("command %q has no help flag", cmd.CommandPath())
		}
		if strings.Contains(flag.Usage, "help for") {
			t.Errorf("command %q help usage = %q, want localized text", cmd.CommandPath(), flag.Usage)
		}
		if !strings.Contains(flag.Usage, cmd.CommandPath()) {
			t.Errorf("command %q help usage = %q, want it to name the command", cmd.CommandPath(), flag.Usage)
		}
	}
}

func mustFindCommand(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, child := range root.Commands() {
		if child.Name() == name {
			return child
		}
	}
	t.Fatalf("root has no command %q", name)
	return nil
}

// commandNames 返回命令树的全部命令路径，深度优先且顺序稳定。
func commandNames(root *cobra.Command) []string {
	var names []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		names = append(names, cmd.CommandPath())
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	sort.Strings(names)
	return names
}
