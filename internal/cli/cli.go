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
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
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
	cwd                    string
	clock                  func() time.Time
	versionSource          versionSourceFunc
	doctorFactory          doctorFactory
	workspaceFactory       workspaceFactory
	workspaceLoggerFactory workspaceLoggerFactory
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
		workspaceFactory: func(layout *config.Layout) (workspaceService, error) {
			return gitrepo.NewService(layout)
		},
		workspaceLoggerFactory: func(
			ctx context.Context,
			layout *config.Layout,
			stderr io.Writer,
			command string,
			operationID string,
			clock func() time.Time,
		) (workspaceLogger, error) {
			return logging.New(
				ctx,
				layout,
				stderr,
				command,
				operationID,
				logging.WithClock(clock),
			)
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
		return diagnosticExit(io, err)
	}
	d := &deps{ctx: ctx, io: io, options: values}

	// 解析：一次性预解析树只用于定位目标、判定输出模式与帮助，随后丢弃。
	call, err := resolveInvocation(newRoot(d), args)
	if err != nil {
		return diagnosticExit(io, err)
	}
	// 分派：只输出帮助的调用不建立协议会话。
	if call.help {
		return renderHelp(io, call)
	}
	// 执行：正式执行树全新未解析，保证每个 flag 只被 Cobra 解析一次。
	return executeTree(ctx, d, io, args, call.mode)
}

// executeTree 在一棵全新的命令树上执行本次调用并返回退出码。
// 退出码来自命令自己写入的 deps.exitCode；只有树本身执行失败才走诊断通道。
func executeTree(ctx context.Context, d *deps, streams IO, args []string, mode outputMode) int {
	root := newRoot(d)
	root.SetOut(mode.outWriter(streams))
	root.SetErr(streams.Err)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		return diagnosticExit(streams, err)
	}
	return d.exitCode
}

// diagnosticExit 把解析或会话建立失败写到 stderr 并返回对应退出码。
// 协议不兼容固定 10，其余按参数错误返回 2；两者都不触碰 stdout、
// 不承诺 hello/result（设计 §6 表格）。
func diagnosticExit(streams IO, err error) int {
	writeDiagnostic(streams, err)
	if errors.Is(err, errProtocolMismatch) {
		return protocol.ExitCodeProtocolMismatch
	}
	return protocol.ExitCodeInvalidArgument
}

// writeDiagnostic 把解析或会话设置错误写到 stderr，不触碰 stdout。
func writeDiagnostic(streams IO, err error) {
	if streams.Err == nil {
		return
	}
	_, _ = fmt.Fprintf(streams.Err, "auto-mas-runtime: %v\n", err)
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
