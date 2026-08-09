package backend

import (
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// EntryError 分类受管入口文件的稳定失败原因。
var (
	ErrEntryNotFound = errors.New("backend entry file not found")
	ErrEntryUnsafe   = errors.New("backend entry file identity is unsafe")
)

// Error 是 backend 向 CLI 暴露的稳定错误，保留错误码、stage、details 与根因。
type Error struct {
	code      protocol.Code
	stage     protocol.Stage
	message   string
	details   map[string]any
	cause     error
	committed bool
}

func (e *Error) Error() string {
	if e == nil {
		return "backend error"
	}
	if e.cause == nil {
		return fmt.Sprintf("%s: %s", e.code, e.message)
	}
	return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
}

// Code 返回稳定协议错误码。
func (e *Error) Code() protocol.Code {
	if e == nil {
		return protocol.CodeInternalError
	}
	return e.code
}

// Stage 返回错误发生的生命周期 stage。
func (e *Error) Stage() protocol.Stage {
	if e == nil {
		return protocol.StageBackendSpawn
	}
	return e.stage
}

// Message 返回给 CLI 的稳定用户文案。
func (e *Error) Message() string {
	if e == nil {
		return "backend error"
	}
	return e.message
}

// Details 返回 details 的防御性副本；没有细节时仍返回空对象。
func (e *Error) Details() map[string]any {
	if e == nil || len(e.details) == 0 {
		return map[string]any{}
	}
	return cloneDetails(e.details)
}

// Unwrap 返回内部根因。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// TerminalStatus 返回 backend 失败对应的生命周期终态。
func (e *Error) TerminalStatus() string {
	if e == nil {
		return "backend_failed"
	}
	return "backend_failed"
}

// Committed 报告监督是否已经越过进程启动/收口提交点。
func (e *Error) Committed() bool {
	return e != nil && e.committed
}

func newError(code protocol.Code, stage protocol.Stage, message string, details map[string]any, cause error) error {
	if message == "" {
		message = "后端监督失败"
	}
	return &Error{
		code:    code,
		stage:   stage,
		message: message,
		details: cloneDetails(details),
		cause:   cause,
	}
}

func newCommittedError(code protocol.Code, stage protocol.Stage, message string, details map[string]any, cause error) error {
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
	copyOf := make(map[string]any, len(details))
	for key, value := range details {
		copyOf[key] = value
	}
	return copyOf
}

func mapDependencyError(stage protocol.Stage, fallback protocol.Code, message string, err error) error {
	if err == nil {
		return nil
	}
	var coded interface {
		Code() protocol.Code
		Stage() protocol.Stage
		Message() string
		Details() map[string]any
	}
	if errors.As(err, &coded) {
		return newError(coded.Code(), coded.Stage(), coded.Message(), coded.Details(), err)
	}
	var codeOnly interface{ Code() protocol.Code }
	if errors.As(err, &codeOnly) {
		return newError(codeOnly.Code(), stage, message, nil, err)
	}
	return newError(fallback, stage, message, nil, err)
}
