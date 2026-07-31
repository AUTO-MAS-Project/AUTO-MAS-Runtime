package state

import (
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// ErrNotFound 表示固定状态文件不存在。
var ErrNotFound = errors.New("state file not found")

// ErrCorrupt 表示状态文件内容损坏或违反 v1 schema。
var ErrCorrupt = errors.New("state file is corrupt")

// ErrUnsupportedSchema 表示状态文件使用当前 Runtime 不支持的 schema。
var ErrUnsupportedSchema = errors.New("state schema is unsupported")

// ErrTransactionChanged 表示条件删除前事务文件身份或内容已经变化。
var ErrTransactionChanged = errors.New("state transaction changed")

// ErrClosed 表示 Store 已经关闭文件能力。
var ErrClosed = errors.New("state store is closed")

// ErrInvalidPID 表示 PID 不能用于 Windows 进程查询。
var ErrInvalidPID = errors.New("pid is invalid")

var (
	errInvalidValue       = errors.New("state value is invalid")
	errMissingField       = errors.New("state field is missing")
	errInvalidJSON        = errors.New("state json is invalid")
	errDuplicateField     = errors.New("state json field is duplicated")
	errStateFileTooLarge  = errors.New("state file exceeds size limit")
	errMutationNotApplied = errors.New("state mutation reported no applied result")
)

// NotFoundError 表示固定逻辑状态文件不存在。
type NotFoundError struct {
	File string
	Path string
}

func (e *NotFoundError) Error() string {
	if e == nil {
		return "state file not found"
	}
	return fmt.Sprintf("state file %q not found at %q", e.File, e.Path)
}

// Is 让 NotFoundError 匹配 ErrNotFound。
func (e *NotFoundError) Is(target error) bool {
	return target == ErrNotFound
}

// CorruptError 表示磁盘状态内容不满足严格 schema。
type CorruptError struct {
	File  string
	Cause error
}

func (e *CorruptError) Error() string {
	if e == nil {
		return "state file is corrupt"
	}
	return fmt.Sprintf("state file %q is corrupt", e.File)
}

// Is 让 CorruptError 匹配 ErrCorrupt。
func (e *CorruptError) Is(target error) bool {
	return target == ErrCorrupt
}

// Unwrap 保留损坏分类下的稳定原因。
func (e *CorruptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ValidationError 表示调用值或已解码字段违反稳定语义。
type ValidationError struct {
	Field string
	Cause error
}

// Error 仅报告稳定字段名，不回显调用值或磁盘内容。
func (e *ValidationError) Error() string {
	if e == nil {
		return "validate state value"
	}
	return fmt.Sprintf("validate state field %q", e.Field)
}

// Unwrap 保留稳定校验原因。
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// UnsupportedSchemaError 表示 schemaVersion 是合法整数但当前版本不支持。
type UnsupportedSchemaError struct {
	File string
	Got  int
}

func (e *UnsupportedSchemaError) Error() string {
	if e == nil {
		return "state schema is unsupported"
	}
	return fmt.Sprintf("state file %q schema %d is unsupported", e.File, e.Got)
}

// Is 让 UnsupportedSchemaError 匹配 ErrUnsupportedSchema。
func (e *UnsupportedSchemaError) Is(target error) bool {
	return target == ErrUnsupportedSchema
}

// ReadError 表示普通状态文件读取 I/O 故障。
type ReadError struct {
	File  string
	Cause error
}

func (e *ReadError) Error() string {
	if e == nil {
		return "read state file"
	}
	return fmt.Sprintf("read state file %q", e.File)
}

// Unwrap 保留底层 filesystem 或 I/O 错误链。
func (e *ReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// WriteError 表示持久化写入或条件删除故障。
type WriteError struct {
	File             string
	Phase            WritePhase
	MutationApplied  bool
	RecoveryRequired bool
	Cause            error
	CleanupError     error
}

func (e *WriteError) Error() string {
	if e == nil {
		return "write state file"
	}
	return fmt.Sprintf("write state file %q during %q", e.File, e.Phase)
}

// Unwrap 同时保留主错误和清理错误。
func (e *WriteError) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.Cause != nil {
		causes = append(causes, e.Cause)
	}
	if e.CleanupError != nil {
		causes = append(causes, e.CleanupError)
	}
	return causes
}

// Code 返回冻结的持久化写错误码。
func (e *WriteError) Code() protocol.Code {
	return protocol.CodeStateWriteFailed
}

// PIDProbeError 表示查询指定 PID 的 Win32 操作失败。
type PIDProbeError struct {
	Operation string
	PID       uint32
	Cause     error
}

func (e *PIDProbeError) Error() string {
	if e == nil {
		return "probe process"
	}
	return fmt.Sprintf("probe process %d during %q", e.PID, e.Operation)
}

// Unwrap 保留 Win32 原因。
func (e *PIDProbeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func validationError(field string) error {
	return &ValidationError{Field: field, Cause: errInvalidValue}
}
