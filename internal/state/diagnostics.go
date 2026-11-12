package state

import (
	"context"
	"errors"
	"strings"
	"syscall"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// DiagnosticDetails 返回状态写入的结构化事实，原始错误与路径只留在本地错误链。
func (e *WriteError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := persistenceDetails(e.File, e.Cause)
	if e.Phase.Valid() {
		details["phase"] = e.Phase.String()
	}
	details["mutationApplied"] = e.MutationApplied
	details["recoveryRequired"] = e.RecoveryRequired
	if e.UnlinkMode == "classic" || e.UnlinkMode == "posix" || e.UnlinkMode == "unknown" {
		details["unlinkMode"] = e.UnlinkMode
	}
	if cleanup := persistenceCauseDetails(e.CleanupError); len(cleanup) > 0 {
		details["cleanup"] = cleanup
	}
	return map[string]any{"persistence": details}
}

// DiagnosticDetails 返回状态损坏分类，不回显文件内容。
func (e *CorruptError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := persistenceDetails(e.File, nil)
	details["phase"], details["failureKind"] = "decode", "corrupt"
	for _, cause := range []struct {
		err    error
		reason string
	}{
		{errInvalidJSON, "invalid_json"}, {errDuplicateField, "duplicate_field"}, {errMissingField, "missing_field"},
		{errStateFileTooLarge, "too_large"}, {errInvalidValue, "invalid_value"},
	} {
		if errors.Is(e.Cause, cause.err) {
			details["reason"] = cause.reason
			break
		}
	}
	var validation *ValidationError
	if errors.As(e.Cause, &validation) {
		if field := stateDiagnosticField(validation.Field); field != "" {
			details["field"] = field
		}
	}
	return map[string]any{"persistence": details}
}

// DiagnosticDetails 返回不支持的 schema 数字，不读取磁盘。
func (e *UnsupportedSchemaError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := persistenceDetails(e.File, nil)
	details["phase"], details["failureKind"], details["got"] = "decode", "unsupported_schema", e.Got
	return map[string]any{"persistence": details}
}

// DiagnosticDetails 返回已知校验字段，不携带调用值。
func (e *ValidationError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := map[string]any{"phase": "validate", "failureKind": "validation"}
	if field := stateDiagnosticField(e.Field); field != "" {
		details["field"] = field
	}
	return map[string]any{"persistence": details}
}

func stateDiagnosticField(value string) string {
	switch value {
	case "pid", "kind", "schemaVersion", "operationId", "command", "stage", "targetVersion", "baseCommit", "targetCommit", "status", "updatedAt", "startedAt", "version", "commit", "toolVersion", "logPath", "snapshot", "store", "ctx", "layout", "openFiles", "marshalIndent", "files", "lastSuccessful", "broken", "branch", "pythonVersion", "uvVersion", "reason":
		return strings.ToLower(value)
	default:
		return ""
	}
}

// DiagnosticDetails 返回进程探针的 Win32 事实，不读取进程命令行。
func (e *PIDProbeError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := map[string]any{"phase": "probe", "failureKind": "pid_probe", "pid": e.PID}
	switch e.Operation {
	case "open-process", "wait-process", "close-handle", "get-process-times":
		details["operation"] = e.Operation
	}
	if primary := persistenceCauseDetails(e.Cause); len(primary) > 0 {
		details["primary"] = primary
	}
	return map[string]any{"persistence": details}
}

// DiagnosticDetails 返回状态读取的逻辑角色与结构化故障，不暴露文件内容。
func (e *ReadError) DiagnosticDetails() map[string]any {
	if e == nil {
		return nil
	}
	details := persistenceDetails(e.File, e.Cause)
	details["phase"] = "read"
	return map[string]any{"persistence": details}
}

func persistenceDetails(file string, cause error) map[string]any {
	details := make(map[string]any)
	if kind := filesystem.StateFileKind(file); kind.Valid() {
		details["component"] = kind.String()
	}
	if primary := persistenceCauseDetails(cause); len(primary) > 0 {
		details["primary"] = primary
	}
	return details
}

func persistenceCauseDetails(err error) map[string]any {
	details := make(map[string]any)
	errnoSource := err
	var fileError *filesystem.FileError
	var operationError *filesystem.Error
	if errors.As(err, &fileError) {
		details["operation"] = fileError.Operation
		errnoSource = fileError.Err
	} else if errors.As(err, &operationError) {
		details["operation"] = operationError.Operation
		errnoSource = operationError.Err
	}
	var errno syscall.Errno
	if errors.As(errnoSource, &errno) {
		details["windowsError"] = uint64(errno)
	}
	for _, classification := range []struct {
		err  error
		kind string
	}{
		{err: filesystem.ErrIdentityChanged, kind: "identity_changed"},
		{err: filesystem.ErrStateRecoveryRequired, kind: "recovery_required"},
		{err: filesystem.ErrPOSIXUnlinkUnsupported, kind: "unlink_unsupported"},
		{err: filesystem.ErrStateGuardTimeout, kind: "guard_timeout"},
		{err: filesystem.ErrUnsafeHardLink, kind: "unsafe_hard_link"},
		{err: context.Canceled, kind: "cancelled"},
		{err: context.DeadlineExceeded, kind: "deadline_exceeded"},
	} {
		if errors.Is(err, classification.err) {
			details["failureKind"] = classification.kind
			break
		}
	}
	if err != nil && len(details) == 0 {
		details["failureKind"] = "unknown"
	}
	return details
}
