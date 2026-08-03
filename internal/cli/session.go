package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// devRuntimeVersion 是 T7.1 正式注入前的稳定占位版本号。
const devRuntimeVersion = "dev"

// sessionSuccess 描述命令成功后的 result 业务字段。
type sessionSuccess struct {
	message string
	details map[string]any
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
	output, err := newProcessOutput(deps)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	runtimeVersion := helloRuntimeVersion(ctx, deps.options.versionSource)
	emitter, err := output.NewEmitter(
		runtimeVersion,
		command,
		[]string{},
		protocol.WithClock(deps.options.clock),
	)
	if err != nil {
		return sessionSetupFailure(deps, err)
	}
	if err := ctx.Err(); err != nil {
		return emitFailure(deps, emitter, stage, err)
	}
	success, err := run(ctx, emitter)
	if err != nil {
		return emitFailure(deps, emitter, stage, err)
	}
	return emitSuccess(deps, emitter, stage, success)
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

func emitSuccess(deps *deps, emitter *protocol.Emitter, stage protocol.Stage, success sessionSuccess) int {
	result := protocol.NewSuccessResult(stage, "succeeded", success.message, success.details)
	if err := emitter.EmitResult(result); err != nil {
		writeDiagnostic(deps.io, err)
		return protocol.ExitCodePreconditionFailed
	}
	return protocol.ExitCodeSuccess
}

func emitFailure(deps *deps, emitter *protocol.Emitter, fallbackStage protocol.Stage, err error) int {
	code, stage, message, details := classifyFailure(err, fallbackStage)
	errorEvent, err := protocol.NewErrorEvent(code, stage, message, details)
	if err != nil {
		writeDiagnostic(deps.io, err)
		return exitCodeFor(code)
	}
	if err := emitter.EmitError(errorEvent); err != nil {
		writeDiagnostic(deps.io, err)
		return exitCodeFor(code)
	}
	status := "failed"
	if code == protocol.CodeOperationCancelled {
		status = "cancelled"
	}
	result := protocol.NewFailureResult(errorEvent, status, message, details)
	if err := emitter.EmitResult(result); err != nil {
		writeDiagnostic(deps.io, err)
		return exitCodeFor(code)
	}
	return exitCodeFor(code)
}

// classifyFailure 把命令错误映射为冻结错误码与中文 message。
// 取消优先映射为 OPERATION_CANCELLED（设计 §13 顺序：context.Canceled 先于
// operationError 匹配，避免被包装的取消被业务错误码吞掉）；实现了
// operationError 的错误保留其四元组；未知内部错误映射为 INTERNAL_ERROR。
//
// 兜底码不用 OUTPUT_WRITE_FAILED：那个码的语义是协议输出通道写失败，用它
// 兜底会把 Runtime 自身的缺陷伪装成输出故障，让排查从一开始就走错方向
// （T3.8 F13d）。真正的输出写失败由各服务显式映射 OUTPUT_WRITE_FAILED。
func classifyFailure(err error, fallbackStage protocol.Stage) (protocol.Code, protocol.Stage, string, map[string]any) {
	if errors.Is(err, context.Canceled) {
		return protocol.CodeOperationCancelled, fallbackStage, "操作已取消", map[string]any{}
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

func exitCodeFor(code protocol.Code) int {
	definition, ok := protocol.LookupErrorDefinition(code)
	if !ok {
		return protocol.ExitCodeInvalidArgument
	}
	return definition.ExitCode
}
