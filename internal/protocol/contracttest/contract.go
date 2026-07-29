// Package contracttest 校验 Runtime 命令发射的原始协议 NDJSON。
package contracttest

import "testing"

// Terminal 标识 transcript 预期的操作终态。
type Terminal string

const (
	TerminalSuccess   Terminal = "success"
	TerminalFailure   Terminal = "failure"
	TerminalCancelled Terminal = "cancelled"
)

// Transcript 保存命令场景写出的原始 stdout 字节。
type Transcript struct {
	Stdout []byte
}

// Runner 执行一个命令场景并返回原始 transcript。
type Runner func(t *testing.T, terminal Terminal) Transcript

// Register 对每一种终态场景运行公共契约。
func Register(t *testing.T, command string, run Runner) {
	t.Helper()
	for _, terminal := range []Terminal{
		TerminalSuccess,
		TerminalFailure,
		TerminalCancelled,
	} {
		terminal := terminal
		t.Run(string(terminal), func(t *testing.T) {
			transcript := run(t, terminal)
			Assert(t, command, terminal, transcript.Stdout)
		})
	}
}

// Assert 报告 stdout 中的每一项契约违规。
func Assert(t *testing.T, command string, terminal Terminal, stdout []byte) {
	t.Helper()
	_, issues := inspect(command, terminal, stdout)
	for _, issue := range issues {
		t.Error(issue.Error())
	}
}
