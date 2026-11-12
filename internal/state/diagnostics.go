package state

import (
	"context"
	"errors"
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
	if cleanup := persistenceCauseDetails(e.CleanupError); len(cleanup) > 0 {
		details["cleanup"] = cleanup
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
