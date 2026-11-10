package backend

import (
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// supervisionDiagnosticError 附带已经校验的监督身份和清理事实，不参与协议错误选择。
type supervisionDiagnosticError struct {
	cause    error
	mode     Mode
	revision state.Revision
}

func (e *supervisionDiagnosticError) Error() string { return e.cause.Error() }
func (e *supervisionDiagnosticError) Unwrap() error { return e.cause }

// DiagnosticDetails 返回供观察器净化的诊断副本，不补查 Git 或用户配置。
func (e *supervisionDiagnosticError) DiagnosticDetails() map[string]any {
	details := map[string]any{"mode": string(e.mode)}
	if e.revision.Version != "" {
		details["version"] = e.revision.Version
	}
	if e.revision.Commit != "" {
		details["commit"] = e.revision.Commit
	}
	var cleanup *cleanupDiagnosticError
	if errors.As(e.cause, &cleanup) {
		details["terminationRequested"] = cleanup.terminationRequested
	}
	return details
}

// cleanupDiagnosticError 将进程终止事实与协议 details 分开，保留原始错误链。
type cleanupDiagnosticError struct {
	cause                error
	terminationRequested bool
}

func (e *cleanupDiagnosticError) Error() string { return e.cause.Error() }
func (e *cleanupDiagnosticError) Unwrap() error { return e.cause }

// failure 按既有清理优先级合并失败，只在诊断通道附带根进程终止事实。
func (c processCleanup) failure(primary ...error) error {
	err := errors.Join(append([]error{c.err}, primary...)...)
	if err == nil {
		return nil
	}
	return &cleanupDiagnosticError{cause: err, terminationRequested: c.rootForced}
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
