package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// workspaceCheckCommand 注册只读 workspace 检查。
func workspaceCheckCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "只读检查受管仓库",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperation(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageWorkspaceCheck,
				func(ctx context.Context, _ *protocol.Emitter) (sessionSuccess, error) {
					service, err := deps.options.workspaceFactory(deps.global.layout)
					if err != nil {
						return sessionSuccess{}, err
					}
					result, err := service.Check(ctx)
					if err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "仓库检查完成",
						details: workspaceCheckDetails(result),
					}, nil
				},
			)
			return nil
		},
	}
}

// workspaceSyncCommand 注册带目标版本和 stdin cancel 的仓库同步。
func workspaceSyncCommand(deps *deps) *cobra.Command {
	command := &cobra.Command{
		Use:   "sync",
		Short: "同步受管仓库到目标版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperationWithCapabilities(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageWorkspaceSwap,
				[]string{string(protocol.CapabilityStdinCancel), string(protocol.CapabilityStateV1)},
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					return runWorkspaceSync(ctx, deps, cmd, emitter)
				},
			)
			return nil
		},
	}
	command.Flags().StringArray("version", nil, "目标版本（例如 v5.4.0-beta.1）")
	return command
}

func runWorkspaceSync(
	ctx context.Context,
	deps *deps,
	command *cobra.Command,
	emitter *protocol.Emitter,
) (sessionSuccess, error) {
	versions, err := depsVersionValues(command)
	if err != nil {
		return sessionSuccess{}, err
	}
	target, err := gitrepo.ParseTarget(versions[0])
	if err != nil {
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeInvalidVersion,
			stage:   protocol.StageWorkspaceClone,
			message: "目标版本无效",
			details: map[string]any{},
			cause:   err,
		}
	}

	controlContext, cancel := context.WithCancel(ctx)
	defer cancel()
	control := newWorkspaceControl(cancel, protocol.StageWorkspaceClone)
	trackedEmitter := &workspaceStageEmitter{emitter: emitter, control: control}
	service, err := deps.options.workspaceFactory(deps.global.layout)
	if err != nil {
		return sessionSuccess{}, err
	}
	reader, err := protocol.NewControlReader(
		deps.io.In,
		emitter,
		control,
		protocol.ControlCancel,
	)
	if err != nil {
		return sessionSuccess{}, workspaceControlInfrastructureError(control.CurrentControlStage(), err)
	}
	controlDone := make(chan error, 1)
	go func() {
		readErr := reader.Run(controlContext)
		if readErr != nil {
			contextStopped := isWorkspaceControlContextCancellation(controlContext, readErr)
			if !contextStopped {
				control.SetReaderError(readErr)
				cancel()
			}
		}
		controlDone <- readErr
	}()
	binding := &workspaceLogBinding{}
	request := gitrepo.SyncRequest{
		Target:      target,
		Policy:      deps.global.mirrorPolicy,
		OperationID: emitter.OperationID(),
		PID:         uint32(os.Getpid()),
		Emitter:     trackedEmitter,
		LoggerFactory: func(
			loggerContext context.Context,
			command string,
			operationID string,
		) (gitrepo.OperationLogger, error) {
			logger, loggerErr := deps.options.workspaceLoggerFactory(
				loggerContext,
				deps.global.layout,
				deps.io.Err,
				command,
				operationID,
				deps.options.clock,
			)
			if loggerErr == nil {
				binding.Set(logger)
			}
			return logger, loggerErr
		},
		Auditor:          binding,
		Clock:            deps.options.clock,
		StageReporter:    control.SetStage,
		ControlCommandID: control.CommandID,
	}
	result, syncErr := service.Sync(controlContext, request)
	cancel()
	stopErr := stopWorkspaceControl(reader, deps.io.In, controlDone)
	if stopErr != nil {
		syncErr = joinWorkspaceControlError(
			syncErr,
			workspaceControlInfrastructureError(control.CurrentControlStage(), stopErr),
		)
	}
	controlErr := control.ReaderError()
	if controlErr != nil {
		syncErr = joinWorkspaceControlError(
			syncErr,
			workspaceControlInfrastructureError(control.CurrentControlStage(), controlErr),
		)
	}
	result = withWorkspaceControlCommandID(result, control.CommandID())
	if syncErr != nil {
		syncErr = addControlCommandID(syncErr, control.CommandID())
		return sessionSuccess{}, syncErr
	}
	return sessionSuccess{
		message: "仓库同步完成",
		status:  string(result.Status),
		details: workspaceSyncDetails(result),
	}, nil
}

const workspaceControlJoinTimeout = time.Second

func stopWorkspaceControl(
	reader *protocol.ControlReader,
	input io.Reader,
	completed <-chan error,
) error {
	reader.StopAccepting()
	if file, ok := input.(*os.File); ok && file == os.Stdin {
		// 真实 stdin 由进程拥有，不能关闭；其阻塞读取由进程终止收口，
		// 当前命令不为不可取消的系统句柄额外等待 join 超时。
		return nil
	}
	var closeErr error
	if closer, ok := ownedControlInput(input); ok {
		// Runtime 独占本次进程的 stdin；关闭它才能让真实控制读取在取消后
		// 退出并完成 join。调用方传入的其他 reader 也必须支持有限时间 Close。
		closeErr = closer.Close()
	}
	timer := time.NewTimer(workspaceControlJoinTimeout)
	defer timer.Stop()
	select {
	case readErr := <-completed:
		if closeErr != nil {
			return errors.Join(closeErr, readErr)
		}
		if readErr == context.Canceled || readErr == context.DeadlineExceeded {
			return nil
		}
		return readErr
	case <-timer.C:
		if closeErr != nil {
			return errors.Join(closeErr, errors.New("stdin control reader did not exit"))
		}
		return errors.New("stdin control reader did not exit")
	}
}

func ownedControlInput(input io.Reader) (io.Closer, bool) {
	if input == nil {
		return nil, false
	}
	if file, ok := input.(*os.File); ok && file == os.Stdin {
		return nil, false
	}
	closer, ok := input.(io.Closer)
	return closer, ok
}

func depsVersionValues(command *cobra.Command) ([]string, error) {
	if command == nil {
		return nil, errors.New("workspace sync command is unavailable")
	}
	values, err := command.Flags().GetStringArray("version")
	if err != nil {
		return nil, err
	}
	if len(values) != 1 || values[0] == "" {
		return nil, &commandError{
			code:    protocol.CodeInvalidVersion,
			stage:   protocol.StageWorkspaceClone,
			message: "目标版本无效",
			details: map[string]any{},
			cause:   errors.New("workspace sync requires exactly one version"),
		}
	}
	return values, nil
}

func workspaceCheckDetails(result gitrepo.CheckResult) map[string]any {
	return map[string]any{
		"healthy": result.Healthy,
		"version": result.Version,
		"branch":  result.Branch,
		"commit":  result.Commit,
		"source":  result.Source,
		"reason":  result.Reason,
	}
}

func workspaceSyncDetails(result gitrepo.SyncResult) map[string]any {
	details := map[string]any{
		"version": result.Revision.Version(),
		"branch":  result.Revision.Branch(),
		"commit":  result.Revision.Commit(),
		"source":  result.Revision.SourceKey(),
		"changed": result.Changed,
	}
	if result.ControlCommandID != "" {
		details["controlCommandId"] = result.ControlCommandID
	}
	return details
}

type workspaceStageEmitter struct {
	emitter *protocol.Emitter
	control *workspaceControl
}

func (e *workspaceStageEmitter) EmitProgress(event protocol.ProgressEvent) error {
	// progress 的 stage 是事件所属阶段，不覆盖由业务动作下传的控制 stage。
	return e.emitter.EmitProgress(event)
}

func (e *workspaceStageEmitter) EmitState(event protocol.StateEvent) error {
	e.control.SetStage(event.Stage)
	return e.emitter.EmitState(event)
}

type workspaceControl struct {
	mu          sync.RWMutex
	cancel      context.CancelFunc
	stage       protocol.Stage
	commandID   string
	readerError error
}

func newWorkspaceControl(cancel context.CancelFunc, stage protocol.Stage) *workspaceControl {
	return &workspaceControl{cancel: cancel, stage: stage}
}

func (c *workspaceControl) PrepareControl(command protocol.ControlCommand) (protocol.ControlDisposition, protocol.ControlAction, error) {
	if command.Command != protocol.ControlCancel {
		return protocol.ControlNotApplicable, nil, nil
	}
	return protocol.ControlAccepted, func() error {
		c.mu.Lock()
		if c.commandID == "" {
			c.commandID = command.CommandID
		}
		cancel := c.cancel
		c.mu.Unlock()
		cancel()
		return nil
	}, nil
}

func (c *workspaceControl) CurrentControlStage() protocol.Stage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stage
}

func (c *workspaceControl) SetStage(stage protocol.Stage) {
	c.mu.Lock()
	c.stage = stage
	c.mu.Unlock()
}

func (c *workspaceControl) CommandID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.commandID
}

func (c *workspaceControl) SetReaderError(err error) {
	c.mu.Lock()
	c.readerError = err
	c.mu.Unlock()
}

func (c *workspaceControl) ReaderError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.readerError
}

func workspaceControlInfrastructureError(stage protocol.Stage, cause error) error {
	code := protocol.CodeInternalError
	message := "stdin 控制通道读取失败"
	if errors.Is(cause, protocol.ErrOutputWriteFailed) {
		code = protocol.CodeOutputWriteFailed
		message = "协议输出失败"
	}
	return &commandError{
		code:                  code,
		stage:                 stage,
		message:               message,
		details:               map[string]any{},
		cause:                 cause,
		controlInfrastructure: true,
	}
}

func withWorkspaceControlCommandID(result gitrepo.SyncResult, commandID string) gitrepo.SyncResult {
	if commandID != "" {
		result.ControlCommandID = commandID
	}
	return result
}

func joinWorkspaceControlError(businessErr, controlErr error) error {
	return errors.Join(controlErr, businessErr)
}

func isWorkspaceControlContextCancellation(controlContext context.Context, readErr error) bool {
	if controlContext == nil || controlContext.Err() == nil {
		return false
	}
	// ControlReader 返回裸 sentinel 表示自身 context 已停止；带 read stdin control 包装的
	// 同名错误来自底层 reader，必须保留为控制通道基础设施故障。
	var expected expectedControlCancellation
	if errors.As(readErr, &expected) {
		return true
	}
	return readErr == context.Canceled || readErr == context.DeadlineExceeded
}

func addControlCommandID(err error, commandID string) error {
	if commandID == "" {
		return err
	}
	code, stage, message, details := classifyFailure(err, protocol.StageWorkspaceCleanup)
	if details == nil {
		details = map[string]any{}
	}
	details["controlCommandId"] = commandID
	return &commandError{
		code:      code,
		stage:     stage,
		message:   message,
		details:   details,
		cause:     err,
		committed: findCommittedOperationError(err) != nil,
	}
}

type workspaceLogBinding struct {
	mu     sync.RWMutex
	logger workspaceLogger
}

func (b *workspaceLogBinding) Set(logger workspaceLogger) {
	b.mu.Lock()
	b.logger = logger
	b.mu.Unlock()
}

func (b *workspaceLogBinding) RecordDeletion(
	ctx context.Context,
	record filesystem.DeleteAuditRecord,
) error {
	b.mu.RLock()
	logger := b.logger
	b.mu.RUnlock()
	if logger == nil {
		return errors.New("workspace logger is unavailable")
	}
	_, err := logger.Record(ctx, logging.LevelInfo, "受管目录删除审计", map[string]any{
		"phase":       record.Phase,
		"kind":        record.Kind,
		"operationId": record.OperationID,
		"target":      record.Target,
		"reason":      record.Reason,
		"removed":     record.Removed,
		"partial":     record.Partial,
		"result":      record.Result,
	})
	return err
}

var _ protocol.ControlHandler = (*workspaceControl)(nil)
var _ gitrepo.WorkspaceEmitter = (*workspaceStageEmitter)(nil)
