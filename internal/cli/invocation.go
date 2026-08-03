package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// invocation 是预解析阶段对一次调用的完整判定：执行哪个命令、用哪种输出模式、
// 是否只输出帮助。
//
// 预解析必须在一棵一次性的命令树上进行，判定结果不带进正式执行：pflag 的
// StringArray Set 语义是「首次赋值、之后追加」，同一 FlagSet 被解析两次会让
// 单个 --mirror 累积成两条并触发重复 kind 校验（T3.6 F1）。
type invocation struct {
	// target 是预解析树中定位到的命令。它只用于读取已解析的 flag 和渲染帮助，
	// 绝不用于执行——执行必须发生在一棵全新未解析的树上。
	target *cobra.Command
	mode   outputMode
	help   bool
}

// resolveInvocation 在一次性预解析树上判定本次调用。
//
// 分支优先级（设计 §6 表格，由 TestResolveInvocation 固定）：
//  1. 命令定位与 flag 解析失败 → 参数错误；
//  2. --output / --protocol 语义校验，先于 --help，保证 --help 不压过它们
//     （T3.5 F5）；协议不兼容返回 errProtocolMismatch（退出码 10）；
//  3. --help → 只输出帮助，不进入协议会话；
//  4. 不可运行的命令（根命令与四个组命令）带多余位置参数 → 参数错误
//     （T3.6 F3）；根级未知命令已由 Find 提前拒绝。
func resolveInvocation(probeRoot *cobra.Command, args []string) (invocation, error) {
	target, remaining, err := probeRoot.Find(args)
	if err != nil {
		return invocation{}, err
	}
	// help flag 由 Cobra 在 execute 内部初始化；预解析不经过 execute，
	// 必须显式建立才能解析 --help。
	target.InitDefaultHelpFlag()
	if err := target.ParseFlags(remaining); err != nil {
		return invocation{}, err
	}
	mode, err := validateOutputAndProtocol(target.Flags())
	if err != nil {
		return invocation{}, err
	}
	call := invocation{target: target, mode: mode}
	// GetBool 只在 flag 不存在时报错，InitDefaultHelpFlag 之后不会发生；
	// 真出现时按「未请求帮助」继续，由后续执行路径给出诊断。
	if help, err := target.Flags().GetBool("help"); err == nil && help {
		call.help = true
		return call, nil
	}
	if !target.Runnable() && len(target.Flags().Args()) > 0 {
		return invocation{}, fmt.Errorf(
			"unknown command %q for %q",
			target.Flags().Args()[0],
			target.Name(),
		)
	}
	return call, nil
}

// renderHelp 按输出模式路由帮助文本并返回退出码。
// ndjson 模式下 stdout 必须保持机器可解析，帮助只允许出现在 stderr。
func renderHelp(streams IO, call invocation) int {
	call.target.Root().SetOut(call.mode.outWriter(streams))
	if err := call.target.Help(); err != nil {
		writeDiagnostic(streams, err)
		return protocol.ExitCodePreconditionFailed
	}
	return protocol.ExitCodeSuccess
}
