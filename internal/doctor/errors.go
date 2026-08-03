package doctor

import (
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Error 描述 doctor 服务失败，供 cli 会话框架映射协议四元组。
// 它实现 cli 侧消费方定义的 operationError 窄接口，是 doctor 与 cli 之间
// 唯一的失败契约：生产路径用于协议事件发射失败（见 newOutputError），
// cli 的注入式测试替身也用它构造服务级失败。
type Error struct {
	code    protocol.Code
	stage   protocol.Stage
	message string
	details map[string]any
	cause   error
}

// NewError 构造 doctor 服务错误。
func NewError(
	code protocol.Code,
	stage protocol.Stage,
	message string,
	details map[string]any,
	cause error,
) *Error {
	return &Error{
		code:    code,
		stage:   stage,
		message: message,
		details: details,
		cause:   cause,
	}
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.message
	}
	return fmt.Sprintf("%s: %v", e.message, e.cause)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Code() protocol.Code { return e.code }

func (e *Error) Stage() protocol.Stage { return e.stage }

func (e *Error) Message() string { return e.message }

func (e *Error) Details() map[string]any { return e.details }
