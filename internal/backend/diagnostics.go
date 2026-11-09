package backend

import (
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// supervisionDiagnosticError 附带已经校验的监督身份，不参与协议错误选择。
type supervisionDiagnosticError struct {
	cause    error
	mode     Mode
	revision state.Revision
}

func (e *supervisionDiagnosticError) Error() string { return e.cause.Error() }
func (e *supervisionDiagnosticError) Unwrap() error { return e.cause }

// DiagnosticDetails 返回供观察器净化的身份副本，不补查 Git 或用户配置。
func (e *supervisionDiagnosticError) DiagnosticDetails() map[string]any {
	details := map[string]any{"mode": string(e.mode)}
	if e.revision.Version != "" {
		details["version"] = e.revision.Version
	}
	if e.revision.Commit != "" {
		details["commit"] = e.revision.Commit
	}
	return details
}

func withSupervisionDiagnostics(err error, mode Mode, revision state.Revision) error {
	if err == nil {
		return nil
	}
	var existing *supervisionDiagnosticError
	if errors.As(err, &existing) {
		return err
	}
	return &supervisionDiagnosticError{cause: err, mode: mode, revision: revision}
}
