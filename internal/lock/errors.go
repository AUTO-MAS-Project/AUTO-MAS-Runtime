package lock

import (
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var (
	// ErrClosed 表示 Set 已开始或完成关闭。
	ErrClosed = errors.New("mutex set is closed")
	// ErrPoisoned 表示此前的 ReleaseMutex 失败使所有权不再确定。
	ErrPoisoned = errors.New("mutex set is poisoned")
)

// ConflictError 描述稳定的正常互斥冲突。
type ConflictError struct {
	code protocol.Code
	Kind Kind
	Name string
}

// Error 返回不含 app root 路径的诊断文本。
func (e *ConflictError) Error() string {
	return fmt.Sprintf(
		"mutex conflict: kind=%q name=%q code=%q",
		e.Kind,
		e.Name,
		e.code,
	)
}

// Code 返回协议层冻结的冲突码。
func (e *ConflictError) Code() protocol.Code {
	return e.code
}

// OperationError 描述一项 Windows Mutex 操作故障。
type OperationError struct {
	code      protocol.Code
	Operation string
	Kind      Kind
	Name      string
	Cause     error
}

// Error 返回不含 app root 路径的诊断文本。
func (e *OperationError) Error() string {
	return fmt.Sprintf(
		"mutex operation: operation=%q kind=%q name=%q: %v",
		e.Operation,
		e.Kind,
		e.Name,
		e.Cause,
	)
}

// Unwrap 返回底层 Windows 或上下文错误。
func (e *OperationError) Unwrap() error {
	return e.Cause
}

// Code 固定返回普通 Mutex 操作故障码。
func (e *OperationError) Code() protocol.Code {
	return protocol.CodeMutexOperationFailed
}

// PoisonedError 保留首次使 Set 中毒的 ReleaseMutex 故障。
type PoisonedError struct {
	Cause error
}

// Error 返回中毒状态及首次原因。
func (e *PoisonedError) Error() string {
	return fmt.Sprintf("mutex set is poisoned: %v", e.Cause)
}

// Is 使 errors.Is 可以识别 ErrPoisoned。
func (e *PoisonedError) Is(target error) bool {
	return target == ErrPoisoned
}

// Unwrap 返回首次 ReleaseMutex 故障。
func (e *PoisonedError) Unwrap() error {
	return e.Cause
}

// Code 固定返回普通 Mutex 操作故障码。
func (e *PoisonedError) Code() protocol.Code {
	return protocol.CodeMutexOperationFailed
}
