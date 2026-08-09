package health

import (
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Error 表示健康检查失败及其稳定协议分类。
type Error struct {
	code    protocol.Code
	stage   protocol.Stage
	message string
	details map[string]any
	cause   error
}

// Error 返回面向诊断日志的稳定错误文本。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.message == "" {
		return string(e.code)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// Code 返回稳定错误码。
func (e *Error) Code() protocol.Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Stage 返回固定的健康检查阶段。
func (e *Error) Stage() protocol.Stage {
	if e == nil {
		return ""
	}
	return e.stage
}

// Message 返回不含错误码的诊断消息。
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Details 返回诊断细节的防御性副本。
func (e *Error) Details() map[string]any {
	if e == nil {
		return nil
	}
	return cloneDetails(e.details)
}

// Unwrap 保留取消、传输或探针的原始错误。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newError(code protocol.Code, message string, details map[string]any, cause error) *Error {
	return &Error{
		code:    code,
		stage:   protocol.StageBackendHealth,
		message: message,
		details: cloneDetails(details),
		cause:   cause,
	}
}

func cloneDetails(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneDetailValue(value)
	}
	return result
}

func cloneDetailValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneDetails(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneDetailValue(item)
		}
		return result
	default:
		return value
	}
}
