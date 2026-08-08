package uv

import "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"

// Error 是 uv、Python 与依赖操作向 CLI 暴露的结构化错误。
type Error struct {
	code      protocol.Code
	stage     protocol.Stage
	message   string
	details   map[string]any
	cause     error
	committed bool
}

// Error 返回不包含外部命令原文的稳定诊断文本。
func (e *Error) Error() string {
	if e == nil {
		return "uv operation failed"
	}
	return e.message
}

// Unwrap 保留可供内部诊断使用的错误链。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Code 返回稳定协议错误码。
func (e *Error) Code() protocol.Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Stage 返回错误所属协议阶段。
func (e *Error) Stage() protocol.Stage {
	if e == nil {
		return ""
	}
	return e.stage
}

// Message 返回面向调用方的中文诊断。
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Details 返回结构化细节的防御性副本。
func (e *Error) Details() map[string]any {
	if e == nil {
		return map[string]any{}
	}
	return cloneDetails(e.details)
}

// Committed 报告错误是否发生在不可逆发布之后。
func (e *Error) Committed() bool {
	return e != nil && e.committed
}

func newError(
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
		details: cloneDetails(details),
		cause:   cause,
	}
}

func newCommittedError(
	code protocol.Code,
	stage protocol.Stage,
	message string,
	details map[string]any,
	cause error,
) *Error {
	return &Error{
		code:      code,
		stage:     stage,
		message:   message,
		details:   cloneDetails(details),
		cause:     cause,
		committed: true,
	}
}

func cloneDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(details))
	for key, value := range details {
		cloned[key] = value
	}
	return cloned
}
