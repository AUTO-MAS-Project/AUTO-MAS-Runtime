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
	root := newRoot(d, io)

	target, remaining, err := root.Find(args)
	if err != nil {
		return parseFailure(io, err)
	}
	// help flag 由 Cobra 在 execute 内初始化；预解析阶段必须先显式建立，
	// 才能判断 --help 并把帮助文本路由到正确通道。
	target.InitDefaultHelpFlag()
	if err := target.ParseFlags(remaining); err != nil {
		return parseFailure(io, err)
	}
	rawOutput, err := target.Flags().GetString("output")
	if err != nil {
		return parseFailure(io, err)
	}
	helpRequested, err := target.Flags().GetBool("help")
	if err == nil && helpRequested {
		// NDJSON 模式的帮助文本只允许出现在 stderr，stdout 必须保持机器可解析。
		if rawOutput == string(outputNDJSON) {
			root.SetOut(io.Err)
		} else {
			root.SetOut(io.Out)
		}
		if err := target.Help(); err != nil {
			writeDiagnostic(io, err)
			return protocol.ExitCodePreconditionFailed
		}
		return protocol.ExitCodeSuccess
	}
	if rawOutput == string(outputNDJSON) {
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
