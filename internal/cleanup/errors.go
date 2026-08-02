package cleanup

import (
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Error 描述 cleanup 服务失败，供 cli 会话框架映射协议四元组。
type Error struct {
	code    protocol.Code
	stage   protocol.Stage
	message string
	details map[string]any
	cause   error
}

// NewError 构造 cleanup 服务错误。
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

// mapLockError 保留 lock 包冲突与操作故障的冻结错误码。
func mapLockError(err error) error {
	var conflict *lock.ConflictError
	if errors.As(err, &conflict) {
		return NewError(
			conflict.Code(),
			protocol.StageCleanup,
			messageForCode(conflict.Code()),
			map[string]any{},
			err,
		)
	}
	return NewError(
		protocol.CodeMutexOperationFailed,
		protocol.StageCleanup,
		"Mutex 操作失败",
		map[string]any{},
		err,
	)
}

// mapOperationError 保留 filesystem 错误码，其余错误使用兜底码。
func mapOperationError(err error, fallback protocol.Code, message string) error {
	var fileErr *filesystem.Error
	if errors.As(err, &fileErr) {
		return NewError(
			fileErr.Code(),
			protocol.StageCleanup,
			messageForCode(fileErr.Code()),
			map[string]any{},
			err,
		)
	}
	return NewError(
		fallback,
		protocol.StageCleanup,
		message,
		map[string]any{},
		err,
	)
}

func messageForCode(code protocol.Code) string {
	switch code {
	case protocol.CodeMutationInProgress:
		return "另一个变更操作正在进行"
	case protocol.CodeBackendStillRunning:
		return "后端仍在运行"
	case protocol.CodeMutexOperationFailed:
		return "Mutex 操作失败"
	case protocol.CodeOperationCancelled:
		return "清理已取消"
	case protocol.CodeGitRepoCleanupFailed:
		return "部分清理项目失败"
	case protocol.CodePathOutsideManagedRoot:
		return "目标超出受管根"
	case protocol.CodeUnsafeReparsePoint:
		return "目标为不安全的链接"
	case protocol.CodeDirectoryOccupied:
		return "目录被占用"
	default:
		return "清理失败"
	}
}
