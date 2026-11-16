package gitrepo

import (
	"errors"
	"os"
	"syscall"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// DiagnosticDetails 返回仓库操作的诊断副本，不改变协议详情。
func (e *Error) DiagnosticDetails() map[string]any {
	if e == nil || len(e.diagnostics) == 0 {
		return nil
	}
	details := make(map[string]any, len(e.diagnostics))
	for key, value := range e.diagnostics {
		if fields, ok := value.(map[string]any); ok {
			copyOf := make(map[string]any, len(fields))
			for name, field := range fields {
				copyOf[name] = field
			}
			value = copyOf
		}
		details[key] = value
	}
	return map[string]any{"repository": details}
}

func withRepositoryRenameDiagnostics(err *Error, kind filesystem.RenameKind, result filesystem.RenameResult) *Error {
	details := repositoryOperationDetails(err.Committed(), err.cause, "rename")
	details["phase"] = kind.String()
	details["mutationApplied"] = result.MutationApplied
	switch kind {
	case filesystem.RenameRepositoryToRetired:
		details["sourceRole"], details["destinationRole"] = "repo", "previous"
	case filesystem.RenameUpdateToRepository:
		details["sourceRole"], details["destinationRole"] = "update", "repo"
	case filesystem.RenameRepositoryRollback:
		details["sourceRole"], details["destinationRole"] = "previous", "repo"
	}
	err.diagnostics = details
	return err
}

func withRepositoryCleanupDiagnostics(err *Error, kind filesystem.DeleteKind, result filesystem.DeleteResult) *Error {
	details := repositoryOperationDetails(err.Committed(), err.cause, "remove_tree")
	switch kind {
	case filesystem.DeleteRepositoryRetired:
		details["component"] = "previous"
	case filesystem.DeleteRepositoryUpdate:
		details["component"] = "update"
	}
	details["removed"], details["partial"], details["auditCompleted"] = result.Removed, result.Partial, result.AuditCompleted
	err.diagnostics = details
	return err
}

func withRepositoryCloseDiagnostics(err *Error, cause error) *Error {
	err.diagnostics = repositoryOperationDetails(err.Committed(), cause, "close_lease")
	err.diagnostics["component"] = "update"
	return err
}

func repositoryOperationDetails(committed bool, cause error, operation string) map[string]any {
	details := map[string]any{"operation": operation, "committed": committed}
	primary := make(map[string]any)
	var fileError *filesystem.FileError
	var operationError *filesystem.Error
	var pathError *os.PathError
	switch {
	case errors.As(cause, &fileError):
		primary["operation"], cause = fileError.Operation, fileError.Err
	case errors.As(cause, &operationError):
		primary["operation"], cause = operationError.Operation, operationError.Err
	case errors.As(cause, &pathError):
		primary["operation"], cause = pathError.Op, pathError.Err
	}
	var errno syscall.Errno
	if errors.As(cause, &errno) {
		primary["windowsError"] = uint64(errno)
	}
	if len(primary) == 0 {
		primary["failureKind"] = "unknown"
	}
	details["primary"] = primary
	return details
}
