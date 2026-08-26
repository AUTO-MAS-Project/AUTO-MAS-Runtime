package cli

import (
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// operationError 是应用服务失败与协议错误事件之间的窄接口。
// 接口在消费方（cli）定义；doctor/cleanup 等服务包各自实现自己的错误类型。
type operationError interface {
	error
	Code() protocol.Code
	Stage() protocol.Stage
	Message() string
	Details() map[string]any
}

// terminalStatusError 允许长驻应用服务为 failure result 提供稳定生命周期终态。
// 未实现该接口的命令继续使用通用 failed/cancelled 状态。
type terminalStatusError interface {
	error
	TerminalStatus() string
}

// committedOperationError 标记已经跨过不可逆提交点的应用服务错误。
type committedOperationError interface {
	operationError
	Committed() bool
}

// rejectPackageIndexOverride 在产生任何副作用之前拒绝显式 package-index 首选。
//
// 架构设计要求 dependencies check/sync/rebuild、bootstrap 和顶层 repair 对显式
// `--mirror package-index=<键>` 返回 INVALID_ARGUMENT，且必须在调用 uv 之前失败关闭。
// uv 侧的 validateRequest 也有同样的检查，但那里已经在「uv 已下载、仓库已同步、
// Python 已安装」之后，一个纯参数错误要付出数百 MB 下载的代价。
// 这里复用完全一致的错误码、消息与 details，因此谁先命中对调用方都是同一个错误。
func rejectPackageIndexOverride(policy mirror.Policy, stage protocol.Stage) error {
	source, ok := policy.Preferred(mirror.KindPackageIndex)
	if !ok {
		return nil
	}
	return &commandError{
		code:    protocol.CodeInvalidArgument,
		stage:   stage,
		message: "锁定依赖不支持覆盖包索引",
		details: map[string]any{
			"sourceKind": mirror.KindPackageIndex.String(),
			"source":     source,
		},
		cause: errors.New("package index override conflicts with locked sources"),
	}
}

// commandError 是 cli 内部使用的通用命令错误，同时承载协议映射字段。
type commandError struct {
	code                  protocol.Code
	stage                 protocol.Stage
	message               string
	details               map[string]any
	cause                 error
	controlInfrastructure bool
	committed             bool
}

func (e *commandError) Error() string {
	if e.cause == nil {
		return e.message
	}
	return fmt.Sprintf("%s: %v", e.message, e.cause)
}

func (e *commandError) Unwrap() error { return e.cause }

func (e *commandError) Code() protocol.Code { return e.code }

func (e *commandError) Stage() protocol.Stage { return e.stage }

func (e *commandError) Message() string { return e.message }

func (e *commandError) Details() map[string]any { return e.details }

func (e *commandError) Committed() bool { return e != nil && e.committed }

func (e *commandError) isControlInfrastructureError() bool {
	return e != nil && e.controlInfrastructure
}

// notImplementedError 表示命令已注册但尚未实现，映射为 UNSUPPORTED_MODE。
type notImplementedError struct {
	stage protocol.Stage
}

func (e notImplementedError) Error() string { return "command not implemented" }

func (e notImplementedError) Code() protocol.Code { return protocol.CodeUnsupportedMode }

func (e notImplementedError) Stage() protocol.Stage { return e.stage }

func (e notImplementedError) Message() string { return "命令尚未实现" }

func (e notImplementedError) Details() map[string]any { return map[string]any{} }
