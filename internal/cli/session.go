package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type workspaceControlContextKey struct{}

func workspaceControlFromContext(ctx context.Context) *workspaceControl {
	if ctx == nil {
		return nil
	}
	control, _ := ctx.Value(workspaceControlContextKey{}).(*workspaceControl)
	return control
}

// devRuntimeVersion 是 T7.1 正式注入前的稳定占位版本号。
const devRuntimeVersion = "dev"

// sessionSuccess 描述命令成功后的 result 业务字段。
type sessionSuccess struct {
	message string
	details map[string]any
	status  string
}

// runOperation 是 hello → 执行 → result 的统一协议会话框架。
// 它创建唯一 ProcessOutput/Emitter，执行命令，并把成功或失败收口为唯一 result。
func runOperation(
	ctx context.Context,
	deps *deps,
	command string,
	stage protocol.Stage,
	run func(context.Context, *protocol.Emitter) (sessionSuccess, error),
) int {
	return runOperationWithCapabilities(ctx, deps, command, stage, nil, run)
}

func runOperationWithCapabilities(
	ctx context.Context,
	deps *deps,
	command string,
	stage protocol.Stage,
	capabilities []string,
	run func(context.Context, *protocol.Emitter) (sessionSuccess, error),
) int {
	return runOperationSession(ctx, deps, command, stage, capabilities, false, run)
}

// runOperationWithStdinCancel 为需要接收 stdin cancel 的 M5 操作创建独立控制读取器。
// workspace sync 保留自己的控制编排，因此不能把控制器嵌套到通用会话中。
func runOperationWithStdinCancel(
	ctx context.Context,
	deps *deps,
	command string,
	stage protocol.Stage,
	capabilities []string,
	run func(context.Context, *protocol.Emitter) (sessionSuccess, error),
) int {
	return runOperationSession(ctx, deps, command, stage, capabilities, true, run)
}

func runOperationSession(
	ctx context.Context,
	deps *deps,
	command string,
	stage protocol.Stage,
	capabilities []string,
	withStdinCancel bool,
	run func(context.Context, *protocol.Emitter) (sessionSuccess, error),
) (exitCode int) {
	runtimeVersion, versionPanicked, versionPanicFrames := helloRuntimeVersionSafely(ctx, deps.options.versionSource)
	telemetryState := newSessionTelemetryState(deps, command, stage, runtimeVersion, versionPanicked, versionPanicFrames)
	var emitter *protocol.Emitter
	defer func() {
		if recover() != nil {
			failure := unexpectedPanicError(stage)
			telemetryState.addPanic(failure, capturePanicFrames())
			if emitter == nil {
				writeDiagnostic(deps.io, errors.New("unexpected panic"))
				exitCode = protocol.ExitCodePreconditionFailed
			} else {
				exitCode, telemetryState.terminalWritten = safeEmitPanicFailure(deps, emitter, stage, failure)
			}
		}
		telemetryState.finish()
	}()

	output, err := newProcessOutput(deps)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	emitter, err = output.NewEmitter(
		runtimeVersion,
		command,
		capabilities,
		protocol.WithClock(deps.options.clock),
	)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	telemetryState.sessionStarted = true
	if err := ctx.Err(); err != nil {
		telemetryState.operationErr = err
		exitCode, telemetryState.terminalWritten = emitFailure(deps, emitter, stage, err)
		return exitCode
	}
	operationContext := ctx
	var cancelOperation context.CancelFunc
	var control *workspaceControl
	var controlReader *protocol.ControlReader
	var controlDone chan error
	if withStdinCancel {
		operationContext, cancelOperation = context.WithCancel(ctx)
		control = newWorkspaceControl(cancelOperation, stage)
		controlReader, err = protocol.NewControlReader(
			deps.io.In,
			emitter,
			control,
			protocol.ControlCancel,
		)
		if err != nil {
			cancelOperation()
			failure := workspaceControlInfrastructureError(stage, err)
			telemetryState.operationErr = failure
			exitCode, telemetryState.terminalWritten = emitFailure(deps, emitter, stage, failure)
			return exitCode
		}
		operationContext = context.WithValue(operationContext, workspaceControlContextKey{}, control)
		controlDone = make(chan error, 1)
		go func() {
			readErr := runControlReaderSafely(operationContext, controlReader)
			if readErr != nil && !isWorkspaceControlContextCancellation(operationContext, readErr) {
				control.SetReaderError(readErr)
				cancelOperation()
			}
			controlDone <- readErr
		}()
	}
	success, err, panicked, panicFrames := invokeSessionRun(stage, func() (sessionSuccess, error) {
		return run(operationContext, emitter)
	})
	if controlReader != nil {
		cancelOperation()
		stopErr := stopWorkspaceControl(controlReader, deps.io.In, controlDone)
		if stopErr != nil {
			err = joinWorkspaceControlError(
				err,
				workspaceControlInfrastructureError(control.CurrentControlStage(), stopErr),
			)
		}
		if controlErr := control.ReaderError(); controlErr != nil {
			err = joinWorkspaceControlError(
				err,
				workspaceControlInfrastructureError(control.CurrentControlStage(), controlErr),
			)
		}
		if err != nil {
			err = addControlCommandID(err, control.CommandID())
		} else if control.CommandID() != "" {
			success.details = protocol.WithControlCommandID(success.details, control.CommandID())
		}
	}
	if err != nil {
		telemetryState.operationErr = err
		if panicked {
			telemetryState.addPanic(err, panicFrames)
		}
		exitCode, telemetryState.terminalWritten = emitFailure(deps, emitter, stage, err)
		return exitCode
	}
	exitCode, telemetryState.terminalWritten = emitSuccess(deps, emitter, stage, success)
	if exitCode != protocol.ExitCodeSuccess {
		telemetryState.operationErr = telemetryOutputFailure(stage)
	}
	return exitCode
}

// helloRuntimeVersion 读取版本来源填充 hello.runtimeVersion；
// 读取失败时回退 dev 占位，不阻断协议会话（version 命令自身会报告该错误）。
func helloRuntimeVersion(ctx context.Context, source versionSourceFunc) string {
	if source == nil {
		return devRuntimeVersion
	}
	info, err := source(ctx)
	if err != nil {
		return devRuntimeVersion
	}
	if info.Version == "" {
		return devRuntimeVersion
	}
	return info.Version
}

func newProcessOutput(deps *deps) (*protocol.ProcessOutput, error) {
	switch deps.global.output {
	case outputNDJSON:
		return protocol.NewProcessOutput(deps.io.Out)
	case outputHuman:
		renderer, err := protocol.NewHumanRenderer(deps.io.Out, deps.io.Err)
		if err != nil {
			return nil, err
		}
		return protocol.NewProcessOutputWithRenderer(renderer)
	default:
		return nil, fmt.Errorf("unsupported output mode %q", deps.global.output)
	}
}

// sessionSetupFailure 发生在协议会话建立阶段，此时只能写 stderr 诊断。
func sessionSetupFailure(deps *deps, err error) int {
	writeDiagnostic(deps.io, err)
	return protocol.ExitCodePreconditionFailed
}

func emitSuccess(deps *deps, emitter *protocol.Emitter, stage protocol.Stage, success sessionSuccess) (int, bool) {
	status := success.status
	if status == "" {
		status = "succeeded"
	}
	result := protocol.NewSuccessResult(stage, status, success.message, success.details)
	if err := emitter.EmitResult(result); err != nil {
		writeDiagnostic(deps.io, err)
		return protocol.ExitCodePreconditionFailed, false
	}
	return protocol.ExitCodeSuccess, true
}

func emitFailure(deps *deps, emitter *protocol.Emitter, fallbackStage protocol.Stage, err error) (int, bool) {
	code, stage, message, details := classifyFailure(err, fallbackStage)
	errorEvent, eventErr := protocol.NewErrorEvent(code, stage, message, details)
	if eventErr != nil {
		writeDiagnostic(deps.io, eventErr)
		return exitCodeFor(code), false
	}
	if emitErr := emitter.EmitError(errorEvent); emitErr != nil {
		writeDiagnostic(deps.io, emitErr)
		return exitCodeFor(code), false
	}
	status := "failed"
	if code == protocol.CodeOperationCancelled {
		status = "cancelled"
	} else {
		var terminal terminalStatusError
		if errors.As(err, &terminal) {
			candidate := protocol.StateStatus(terminal.TerminalStatus())
			if protocol.IsKnownStateStatus(candidate) {
				status = string(candidate)
			}
		}
	}
	result := protocol.NewFailureResult(errorEvent, status, message, details)
	if emitErr := emitter.EmitResult(result); emitErr != nil {
		writeDiagnostic(deps.io, emitErr)
		return exitCodeFor(code), false
	}
	return exitCodeFor(code), true
}

// classifyFailure 把命令错误映射为冻结错误码与中文 message。
// 已类型化或原始协议输出故障的 OUTPUT_WRITE_FAILED 优先于取消；已跨提交点的
// 类型化错误随后保留实际持久化事实；除此之外取消优先于业务码，避免被包装的取消
// 被业务错误码吞掉。实现了 operationError 的错误保留其四元组；
// 未知内部错误映射为 INTERNAL_ERROR。
//
// 兜底码不用 OUTPUT_WRITE_FAILED：那个码的语义是协议输出通道写失败，用它
// 兜底会把 Runtime 自身的缺陷伪装成输出故障，让排查从一开始就走错方向
// （T3.8 F13d）。服务可显式映射以保留精确 stage；会话层仍识别原始协议输出故障，
// 避免 version/cleanup 等直接发射 progress 的路径退化成 INTERNAL_ERROR。
func classifyFailure(err error, fallbackStage protocol.Stage) (protocol.Code, protocol.Stage, string, map[string]any) {
	if outputErr := findOperationErrorCode(err, protocol.CodeOutputWriteFailed); outputErr != nil {
		return outputErr.Code(), outputErr.Stage(), outputErr.Message(), outputErr.Details()
	}
	if errors.Is(err, protocol.ErrOutputWriteFailed) {
		return protocol.CodeOutputWriteFailed, fallbackStage, "协议输出失败", map[string]any{}
	}
	if controlErr := findControlInfrastructureError(err); controlErr != nil {
		return controlErr.Code(), controlErr.Stage(), controlErr.Message(), controlErr.Details()
	}
	if committedErr := findCommittedOperationError(err); committedErr != nil {
		code := committedErr.Code()
		if protocol.IsKnownCode(code) {
			return code, committedErr.Stage(), committedErr.Message(), committedErr.Details()
		}
	}
	// 独立 cleanup context 的 deadline 需要保留 workspace.cleanup 的清理事实；
	// 其他阶段（例如 doctor）即使业务码恰好是 cleanup_failed，仍遵循取消优先。
	if cleanupErr := findOperationErrorCode(err, protocol.CodeGitRepoCleanupFailed); cleanupErr != nil &&
		cleanupErr.Stage() == protocol.StageWorkspaceCleanup {
		var primary operationError
		if !errors.As(err, &primary) || primary.Code() == protocol.CodeGitRepoCleanupFailed {
			return cleanupErr.Code(), cleanupErr.Stage(), cleanupErr.Message(), cleanupErr.Details()
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		stage := fallbackStage
		details := map[string]any{}
		var operationErr operationError
		if errors.As(err, &operationErr) {
			if protocol.IsKnownStage(operationErr.Stage()) {
				stage = operationErr.Stage()
			}
			for key, value := range operationErr.Details() {
				details[key] = value
			}
		}
		return protocol.CodeOperationCancelled, stage, "操作已取消", details
	}
	var operationErr operationError
	if errors.As(err, &operationErr) {
		code := operationErr.Code()
		if protocol.IsKnownCode(code) {
			return code, operationErr.Stage(), operationErr.Message(), operationErr.Details()
		}
	}
	return protocol.CodeInternalError, fallbackStage, "命令执行失败", map[string]any{}
}

func findCommittedOperationError(err error) operationError {
	if err == nil {
		return nil
	}
	var selected operationError
	var committedErr committedOperationError
	if errors.As(err, &committedErr) && committedErr.Committed() {
		selected = committedErr
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			selected = selectCommittedOperationError(selected, findCommittedOperationError(child))
		}
		return selected
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		selected = selectCommittedOperationError(selected, findCommittedOperationError(single.Unwrap()))
	}
	return selected
}

func selectCommittedOperationError(current, candidate operationError) operationError {
	if current == nil {
		return candidate
	}
	if candidate == nil {
		return current
	}
	if current.Stage() != protocol.StageBackendCleanup && candidate.Stage() == protocol.StageBackendCleanup {
		return candidate
	}
	return current
}

func findControlInfrastructureError(err error) operationError {
	if err == nil {
		return nil
	}
	var marked interface {
		operationError
		isControlInfrastructureError() bool
	}
	if errors.As(err, &marked) && marked.isControlInfrastructureError() {
		return marked
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			if found := findControlInfrastructureError(child); found != nil {
				return found
			}
		}
		return nil
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return findControlInfrastructureError(single.Unwrap())
	}
	return nil
}

func findOperationErrorCode(err error, want protocol.Code) operationError {
	if err == nil {
		return nil
	}
	var operationErr operationError
	if errors.As(err, &operationErr) && operationErr.Code() == want {
		return operationErr
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			if found := findOperationErrorCode(child, want); found != nil {
				return found
			}
		}
		return nil
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return findOperationErrorCode(single.Unwrap(), want)
	}
	return nil
}

func exitCodeFor(code protocol.Code) int {
	definition, ok := protocol.LookupErrorDefinition(code)
	if !ok {
		return protocol.ExitCodeInvalidArgument
	}
	return definition.ExitCode
}
