package filesystem

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var (
	ErrInvalidArgument            = errors.New("filesystem argument is invalid")
	ErrClosed                     = errors.New("filesystem capability is closed")
	ErrInvalidToken               = errors.New("filesystem token is invalid")
	ErrIdentityChanged            = errors.New("filesystem object identity changed")
	ErrDestinationExists          = errors.New("filesystem destination exists")
	ErrUnsafeHardLink             = errors.New("filesystem hard link is unsafe")
	ErrStateFileNotFound          = errors.New("state file not found")
	ErrStateFileTooLarge          = errors.New("state file exceeds size limit")
	ErrStateRecoveryRequired      = errors.New("state file recovery is required")
	ErrPOSIXUnlinkUnsupported     = errors.New("filesystem POSIX unlink is unsupported")
	ErrUnsupportedCaseSensitivity = errors.New("filesystem case-sensitive directory is unsupported")
)

// Error 携带可映射到协议层的文件系统错误码。
type Error struct {
	code      protocol.Code
	Operation string
	Path      string
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %q: %v", e.Operation, e.Path, e.Err)
}

func (e *Error) Unwrap() error       { return e.Err }
func (e *Error) Code() protocol.Code { return e.code }

// FileError 为单个文件系统操作补充操作名和路径上下文。
type FileError struct {
	Operation string
	Path      string
	Err       error
}

func (e *FileError) Error() string {
	return fmt.Sprintf("%s %q: %v", e.Operation, e.Path, e.Err)
}

func (e *FileError) Unwrap() error { return e.Err }

type operationCleanupError struct {
	cause   error
	cleanup error
}

func (e *operationCleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup: %v", e.cause, e.cleanup)
}

func (e *operationCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.cause, e.cleanup}
}

func joinOperationCleanup(cause, cleanup error) error {
	if cause == nil {
		return cleanup
	}
	if cleanup == nil {
		return cause
	}
	if combined, ok := cause.(*operationCleanupError); ok {
		return &operationCleanupError{
			cause:   combined.cause,
			cleanup: errors.Join(combined.cleanup, cleanup),
		}
	}
	return &operationCleanupError{cause: cause, cleanup: cleanup}
}

func splitOperationCleanup(err error) (error, error) {
	if combined, ok := err.(*operationCleanupError); ok {
		return combined.cause, combined.cleanup
	}
	return err, nil
}

// DeleteKind 限制受控删除可接受的目标类别。
type DeleteKind string

const (
	DeleteUVCache           DeleteKind = "uv_cache"
	DeleteManagedVenv       DeleteKind = "managed_venv"
	DeleteManagedPython     DeleteKind = "managed_python"
	DeleteRepositoryUpdate  DeleteKind = "repository_update"
	DeleteRepositoryRetired DeleteKind = "repository_retired"
	DeleteDownloadTemporary DeleteKind = "download_temporary"
	DeleteUVStaging         DeleteKind = "uv_staging"
	DeletePythonCache       DeleteKind = "python_cache"
	DeleteBuildCache        DeleteKind = "build_cache"
)

func (k DeleteKind) String() string { return string(k) }

func (k DeleteKind) Valid() bool {
	switch k {
	case DeleteUVCache,
		DeleteManagedVenv,
		DeleteManagedPython,
		DeleteRepositoryUpdate,
		DeleteRepositoryRetired,
		DeleteDownloadTemporary,
		DeleteUVStaging,
		DeletePythonCache,
		DeleteBuildCache:
		return true
	default:
		return false
	}
}

// DeleteRequest 描述一次受控递归删除请求。
type DeleteRequest struct {
	Kind        DeleteKind
	Target      string
	OperationID string
	Version     string
	Reason      string
}

// DeleteResult 报告删除副作用与审计收口事实。
type DeleteResult struct {
	Removed        bool
	Partial        bool
	AuditCompleted bool
}

// Auditor 记录受控删除的双阶段审计事件。
type Auditor interface {
	RecordDeletion(ctx context.Context, record DeleteAuditRecord) error
}

// Operator 执行受管删除与原子重命名操作。
type Operator struct {
	layout          *config.Layout
	auditor         Auditor
	api             pathAPI
	wait            WaitFunc
	delays          []time.Duration
	finishedContext func(context.Context) (context.Context, context.CancelFunc)
}

// Option 配置 Operator 的可注入等待策略。
type Option func(*options) error

type options struct {
	wait      WaitFunc
	delays    []time.Duration
	waitSet   bool
	delaysSet bool
}

type operatorDependencies struct {
	api             pathAPI
	finishedContext func(context.Context) (context.Context, context.CancelFunc)
}

// DeleteAuditPhase 标识删除审计的阶段。
type DeleteAuditPhase string

const (
	DeleteAuditStarted  DeleteAuditPhase = "started"
	DeleteAuditFinished DeleteAuditPhase = "finished"
)

func (p DeleteAuditPhase) String() string { return string(p) }

func (p DeleteAuditPhase) Valid() bool {
	switch p {
	case DeleteAuditStarted, DeleteAuditFinished:
		return true
	default:
		return false
	}
}

// DeleteAuditRecord 保存一次删除审计事件的稳定字段。
type DeleteAuditRecord struct {
	Phase       DeleteAuditPhase
	OperationID string
	Kind        DeleteKind
	Target      string
	Reason      string
	Removed     bool
	Partial     bool
	Result      string
}

// AuditError 报告审计阶段及副作用是否已经发生。
type AuditError struct {
	Phase           DeleteAuditPhase
	MutationApplied bool
	Cause           error
}

func (e *AuditError) Error() string {
	return fmt.Sprintf("record %s deletion audit: %v", e.Phase, e.Cause)
}

func (e *AuditError) Unwrap() error { return e.Cause }
