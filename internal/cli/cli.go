// Package cli 解析命令并把工作委派给应用服务。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

// IO 显式注入单次执行的 stdin、stdout 与 stderr。
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type options struct {
	cwd           string
	clock         func() time.Time
	versionSource versionSourceFunc
	doctorFactory doctorFactory
}

// Option 配置 Execute 的可注入测试依赖。
type Option func(*options) error

// WithCWD 注入相对 --app-root 的解析基准目录。
func WithCWD(cwd string) Option {
	return func(values *options) error {
		if cwd == "" {
			return errors.New("cli cwd must not be empty")
		}
		values.cwd = cwd
		return nil
	}
}

// WithClock 注入协议事件时钟，主要供测试使用。
func WithClock(clock func() time.Time) Option {
	return func(values *options) error {
		if clock == nil {
			return errors.New("cli clock must not be nil")
		}
		values.clock = clock
		return nil
	}
}

func applyOptions(values ...Option) (options, error) {
	result := options{
		cwd:           mustGetwd(),
		clock:         time.Now,
		versionSource: version.Load,
		doctorFactory: func(layout *config.Layout, probes doctor.Probes) (doctorService, error) {
			return doctor.New(layout, probes)
		},
	}
	for _, option := range values {
		if option == nil {
			return options{}, errors.New("cli option must not be nil")
		}
		if err := option(&result); err != nil {
			return options{}, err
		}
	}
	return result, nil
}

// deps 保存单次 Execute 的执行状态；Execute 每次调用创建独立实例。
type deps struct {
	ctx      context.Context
	io       IO
	options  options
	global   globalOptions
	exitCode int
}

// Execute 执行一次 Runtime 顶层命令并返回进程退出码。
// 解析失败时向 stderr 写诊断并返回 2；协议不兼容返回 10；
// 解析成功后统一进入 hello → 执行 → result 生命周期。
func Execute(ctx context.Context, args []string, io IO, optionValues ...Option) int {
	if ctx == nil {
		ctx = context.Background()
	}
	values, err := applyOptions(optionValues...)
	if err != nil {
		writeDiagnostic(io, err)
		return protocol.ExitCodeInvalidArgument
	}
	d := &deps{ctx: ctx, io: io, options: values}

	// 预解析树与正式执行树必须是两个独立实例：预解析树只用于定位目标、
	// 探测 --help 与 output/protocol 预校验，随后即被丢弃；正式执行树
	// 全新未解析，保证每个 flag 只被解析一次。否则 pflag StringArray 的
	// Set 语义（首次赋值、之后追加）会让单个 --mirror 值在二次解析时
	// 累积成两条，触发重复 kind 校验（T3.6 F1）。
	probeRoot := newRoot(d, io)
	target, remaining, err := probeRoot.Find(args)
	if err != nil {
		return parseFailure(io, err)
	}
	// help flag 由 Cobra 在 execute 内初始化；预解析阶段必须先显式建立，
	// 才能判断 --help 并把帮助文本路由到正确通道。
	target.InitDefaultHelpFlag()
	if err := target.ParseFlags(remaining); err != nil {
		return parseFailure(io, err)
	}
	// --help 不得压过 --output/--protocol 的语义校验（设计 §6 表格）：
	// 进入 help 分支前先校验，非法 output → 2、协议不兼容 → 10，均不输出帮助。
	mode, err := validateOutputAndProtocol(target.Flags())
	if err != nil {
		if errors.Is(err, errProtocolMismatch) {
			writeDiagnostic(io, err)
			return protocol.ExitCodeProtocolMismatch
		}
		return parseFailure(io, err)
	}
	helpRequested, err := target.Flags().GetBool("help")
	if err == nil && helpRequested {
		// NDJSON 模式的帮助文本只允许出现在 stderr，stdout 必须保持机器可解析。
		if mode == outputNDJSON {
			probeRoot.SetOut(io.Err)
		} else {
			probeRoot.SetOut(io.Out)
		}
		if err := target.Help(); err != nil {
			writeDiagnostic(io, err)
			return protocol.ExitCodePreconditionFailed
		}
		return protocol.ExitCodeSuccess
	}
	// 组命令（不可运行）带未知子命令/多余位置参数按参数错误处理
	// （T3.6 F3 决策）：stderr 诊断 + exit 2，不输出帮助、不发射协议事件；
	// --help 已在上方优先返回，因此 workspace foo --help 仍走帮助路径。
	// 根级未知命令由 probeRoot.Find 提前报错，不会到达这里。
	if !target.Runnable() && len(target.Flags().Args()) > 0 {
		return parseFailure(io, fmt.Errorf(
			"unknown command %q for %q",
			target.Flags().Args()[0],
			target.Name(),
		))
	}
	// 正式执行树：与预解析树同构但全新未解析，Cobra 内部只解析一次，
	// 不继承预解析产生的任何 flag 状态。
	root := newRoot(d, io)
	if mode == outputNDJSON {
		root.SetOut(io.Err)
	} else {
		root.SetOut(io.Out)
	}
	root.SetErr(io.Err)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, errProtocolMismatch) {
			writeDiagnostic(io, err)
			return protocol.ExitCodeProtocolMismatch
		}
		return parseFailure(io, err)
	}
	return d.exitCode
}

func parseFailure(io IO, err error) int {
	writeDiagnostic(io, err)
	return protocol.ExitCodeInvalidArgument
}

// writeDiagnostic 把解析或会话设置错误写到 stderr，不触碰 stdout。
func writeDiagnostic(io IO, err error) {
	if io.Err == nil {
		return
	}
	_, _ = fmt.Fprintf(io.Err, "auto-mas-runtime: %v\n", err)
}

// mustGetwd 返回进程当前目录；Getwd 失败时回退到 "."，
// 该场景下后续 Layout 构造会以非绝对 base 报错，不会产生不安全路径。
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
