package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	backendModeManaged     = "managed"
	backendModeDevelopment = "development"
)

func backendSuperviseCommand(deps *deps) *cobra.Command {
	var mode string
	var repo string
	command := &cobra.Command{
		Use:   "supervise",
		Short: "启动并监督后端进程",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps.exitCode = runBackendSuperviseSession(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageBackendSpawn,
				func(ctx context.Context, emitter *protocol.Emitter, mailbox *backend.ControlMailbox, control *backendControl) (sessionSuccess, error) {
					if mode == "" {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInvalidArgument,
							stage:   protocol.StageBackendSpawn,
							message: "必须显式指定后端运行模式",
							details: map[string]any{"field": "mode"},
							cause:   errors.New("backend mode is required"),
						}
					}
					if mode != backendModeManaged && mode != backendModeDevelopment {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeUnsupportedMode,
							stage:   protocol.StageBackendSpawn,
							message: "当前后端运行模式尚不受支持",
							details: map[string]any{"mode": mode},
							cause:   errors.New("backend mode is unsupported"),
						}
					}
					if mode == backendModeDevelopment {
						if strings.TrimSpace(repo) == "" {
							return sessionSuccess{}, &commandError{
								code:    protocol.CodeInvalidArgument,
								stage:   protocol.StageBackendSpawn,
								message: "开发模式必须指定源码目录",
								details: map[string]any{"field": "repo"},
								cause:   errors.New("development repository is required"),
							}
						}
						if !filepath.IsAbs(repo) {
							repo = filepath.Join(deps.options.cwd, repo)
						}
						repo = filepath.Clean(repo)
					} else if strings.TrimSpace(repo) != "" {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInvalidArgument,
							stage:   protocol.StageBackendSpawn,
							message: "managed 模式不接受 --repo",
							details: map[string]any{"field": "repo", "mode": mode},
							cause:   errors.New("managed mode does not accept development repository"),
						}
					}
					service, err := deps.options.backendFactory(
						ctx,
						deps.global.layout,
						deps.io.Err,
						deps.options.clock,
					)
					if err != nil {
						return sessionSuccess{}, err
					}
					if service == nil {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInternalError,
							stage:   protocol.StageBackendSpawn,
							message: "后端监督器初始化失败",
							details: map[string]any{},
							cause:   errors.New("backend service is nil"),
						}
					}
					pid := os.Getpid()
					if pid <= 0 {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInternalError,
							stage:   protocol.StageBackendSpawn,
							message: "Runtime 进程身份不可用",
							details: map[string]any{},
							cause:   errors.New("runtime pid is invalid"),
						}
					}
					if err := service.Supervise(ctx, backend.Request{
						OperationID:        emitter.OperationID(),
						RuntimePID:         uint32(pid),
						Mode:               backend.Mode(mode),
						DevelopmentRepo:    repo,
						Emitter:            &backendEventEmitter{emitter: emitter, control: control},
						Control:            mailbox,
						BeforeShutdown:     mailbox.BeforeShutdown,
						BeforeControlClose: control.BeforeControlClose,
					}); err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "后端监督已停止",
						details: map[string]any{},
						status:  string(protocol.StateStopped),
					}, nil
				},
			)
			return nil
		},
	}
	command.Flags().StringVar(&mode, "mode", "", "后端运行模式：managed 或 development")
	command.Flags().StringVar(&repo, "repo", "", "development 模式源码目录")
	return command
}

func runBackendSuperviseSession(
	ctx context.Context,
	deps *deps,
	command string,
	stage protocol.Stage,
	run func(context.Context, *protocol.Emitter, *backend.ControlMailbox, *backendControl) (sessionSuccess, error),
) int {
	output, err := newProcessOutput(deps)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	runtimeVersion := helloRuntimeVersion(ctx, deps.options.versionSource)
	emitter, err := output.NewEmitter(
		runtimeVersion,
		command,
		[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1), string(protocol.CapabilityLogStream)},
		protocol.WithClock(deps.options.clock),
	)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	if err := ctx.Err(); err != nil {
		return emitFailure(deps, emitter, stage, err)
	}
	operationContext, cancel := context.WithCancel(ctx)
	defer cancel()
	readerContext, cancelReader := context.WithCancel(operationContext)
	defer cancelReader()
	mailbox := backend.NewControlMailbox(64)
	control := newBackendControl(mailbox, stage, func(command protocol.ControlCommand) error {
		return mailbox.Submit(readerContext, command)
	})
	mailbox.SetStageCallback(control.SetStage)
	reader, err := protocol.NewControlReader(
		deps.io.In,
		emitter,
		control,
		protocol.ControlCancel,
		protocol.ControlShutdown,
		protocol.ControlStatus,
	)
	if err != nil {
		return emitFailure(deps, emitter, stage, backendControlInfrastructureError(stage, err))
	}
	mailbox.SetBeforeShutdown(func(string) {
		cancelReader()
		mailbox.StopAccepting()
		reader.StopAccepting()
	})
	control.SetBeforeControlClose(func() {
		cancelReader()
		mailbox.StopAccepting()
		reader.StopAccepting()
	})
	controlDone := make(chan error, 1)
	go func() {
		readErr := reader.Run(readerContext)
		if readErr != nil && !isWorkspaceControlContextCancellation(readerContext, readErr) {
			control.SetReaderError(readErr)
		}
		controlDone <- readErr
	}()
	success, runErr := run(operationContext, emitter, mailbox, control)
	cancelReader()
	reader.StopAccepting()
	mailbox.StopAccepting()
	cancel()
	stopErr := stopWorkspaceControl(reader, deps.io.In, controlDone)
	mailbox.Close()
	if stopErr != nil {
		runErr = joinWorkspaceControlError(runErr, backendControlInfrastructureError(control.CurrentControlStage(), stopErr))
	}
	if readerErr := control.ReaderError(); readerErr != nil {
		runErr = joinWorkspaceControlError(runErr, backendControlInfrastructureError(control.CurrentControlStage(), readerErr))
	}
	if commandID := control.CommandID(); commandID != "" {
		if runErr != nil {
			runErr = addControlCommandID(runErr, commandID)
		} else {
			success.details = protocol.WithControlCommandID(success.details, commandID)
		}
	}
	if runErr != nil {
		return emitFailure(deps, emitter, stage, runErr)
	}
	return emitSuccess(deps, emitter, stage, success)
}

type backendControl struct {
	mu                 sync.RWMutex
	mailbox            *backend.ControlMailbox
	submit             func(protocol.ControlCommand) error
	stage              protocol.Stage
	commandID          string
	readerErr          error
	beforeControlClose func()
}

func (c *backendControl) SetBeforeControlClose(callback func()) {
	c.mu.Lock()
	c.beforeControlClose = callback
	c.mu.Unlock()
}

func (c *backendControl) BeforeControlClose() {
	c.mu.RLock()
	callback := c.beforeControlClose
	c.mu.RUnlock()
	if callback != nil {
		callback()
	}
}

func newBackendControl(mailbox *backend.ControlMailbox, stage protocol.Stage, submit func(protocol.ControlCommand) error) *backendControl {
	return &backendControl{mailbox: mailbox, submit: submit, stage: stage}
}

func (c *backendControl) PrepareControl(command protocol.ControlCommand) (protocol.ControlDisposition, protocol.ControlAction, error) {
	return protocol.ControlAccepted, func() error {
		if err := c.submit(command); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return expectedControlCancellation{cause: err}
			}
			return err
		}
		if command.Command == protocol.ControlCancel || command.Command == protocol.ControlShutdown {
			c.mu.Lock()
			if c.commandID == "" {
				c.commandID = command.CommandID
			}
			c.mu.Unlock()
		}
		return nil
	}, nil
}

// StopAfterShutdown 保留 cancel-first 后续命令进入 mailbox 的机会；shutdown-first
// 仍沿用协议默认的 reader 停止语义。
func (c *backendControl) StopAfterShutdown(protocol.ControlCommand) bool {
	if c == nil || c.mailbox == nil {
		return true
	}
	if terminal, ok := c.mailbox.TerminalCommand(); ok && terminal.Command == protocol.ControlCancel {
		return false
	}
	return true
}

type expectedControlCancellation struct{ cause error }

func (e expectedControlCancellation) Error() string { return e.cause.Error() }

func (e expectedControlCancellation) Unwrap() error { return e.cause }

func (c *backendControl) CurrentControlStage() protocol.Stage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stage
}

func (c *backendControl) SetStage(stage protocol.Stage) {
	c.mu.Lock()
	c.stage = stage
	c.mu.Unlock()
}

func (c *backendControl) CommandID() string {
	c.mu.RLock()
	commandID := c.commandID
	c.mu.RUnlock()
	if commandID != "" {
		return commandID
	}
	if command, ok := c.mailbox.TerminalCommand(); ok {
		return command.CommandID
	}
	return ""
}

func (c *backendControl) SetReaderError(err error) {
	c.mailbox.SetReaderError(err)
	c.mu.Lock()
	c.readerErr = err
	c.mu.Unlock()
}

func (c *backendControl) ReaderError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.readerErr
}

func backendControlInfrastructureError(stage protocol.Stage, cause error) error {
	return &commandError{
		code:                  protocol.CodeInternalError,
		stage:                 stage,
		message:               "stdin 控制通道读取失败",
		details:               map[string]any{},
		cause:                 cause,
		controlInfrastructure: true,
	}
}

type backendEventEmitter struct {
	emitter *protocol.Emitter
	control *backendControl
}

func (e *backendEventEmitter) EmitState(event protocol.StateEvent) error {
	e.control.SetStage(event.Stage)
	return e.emitter.EmitState(event)
}

func (e *backendEventEmitter) EmitLog(event protocol.LogEvent) error {
	return e.emitter.EmitLog(event)
}

func (e *backendEventEmitter) EmitWarning(event protocol.WarningEvent) error {
	return e.emitter.EmitWarning(event)
}
