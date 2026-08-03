package cleanup

import (
	"context"
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

// newCancelledError 构造取消错误。取消在四处产生（取锁前、取锁后、条目边界、
// lock/filesystem 错误映射），四处必须给出同一组四元组与文案，因此收敛到这里。
func newCancelledError(details map[string]any, cause error) *Error {
	return NewError(
		protocol.CodeOperationCancelled,
		protocol.StageCleanup,
		messageForCode(protocol.CodeOperationCancelled),
		details,
		cause,
	)
}

// mapLockError 保留 lock 包冲突与操作故障的冻结错误码。
func mapLockError(err error) error {
	// 获取前/处理中取消优先映射 OPERATION_CANCELLED（设计 §13 顺序）。
	if errors.Is(err, context.Canceled) {
		return newCancelledError(map[string]any{}, err)
	}
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
		messageForCode(protocol.CodeMutexOperationFailed),
		map[string]any{},
		err,
	)
}

// mapOperationError 保留 filesystem 错误码，其余错误使用兜底码。
func mapOperationError(err error, fallback protocol.Code, message string) error {
	// 取消优先于任何业务分类（设计 §13 顺序），被包装的 ctx 取消也必须命中 130。
	if errors.Is(err, context.Canceled) {
		return newCancelledError(map[string]any{}, err)
	}
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

// messageForCode 是 cleanup 内「错误码 → 中文文案」的唯一来源。
// 此前条目级失败另有一份 safeFailureMessage，与本表重复三条
// （PATH_OUTSIDE_MANAGED_ROOT / UNSAFE_REPARSE_POINT / DIRECTORY_OCCUPIED），
// 改动文案要同时动两处（T3.8 F10）。
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

// messageForItemFailure 返回单条清理目标失败时的中文原因。
//
// 共享文案全部取自 messageForCode，只有兜底不同：条目级失败描述的是这一个
// 目标没删掉（「删除失败」），而 messageForCode 的兜底面向整条命令
// （「清理失败」）。把差异收敛成这一个 default 分支，共享部分不再重复。
func messageForItemFailure(code protocol.Code) string {
	switch code {
	case protocol.CodePathOutsideManagedRoot,
		protocol.CodeUnsafeReparsePoint,
		protocol.CodeDirectoryOccupied:
		return messageForCode(code)
	default:
		return "删除失败"
	}
}
