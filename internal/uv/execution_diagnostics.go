package uv

import (
	"errors"
	"syscall"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const maxNetworkDiagnosticAttempts = 4

func executionDiagnostics(result UVResult, cause error, phase string) map[string]any {
	kind := phase + "_failed"
	if result.ExitCode != 0 {
		kind = phase + "_nonzero"
	}
	details := uvFailureDetails(result, kind)
	var execution *Error
	if errors.As(cause, &execution) {
		for key, value := range execution.Details() {
			details[key] = value
		}
		for key, value := range execution.DiagnosticDetails() {
			details[key] = value
		}
	}
	return details
}

func uvFailureDetails(result UVResult, failureKind string) map[string]any {
	return map[string]any{"failureKind": failureKind, "exitCode": result.ExitCode,
		"durationMs": result.Duration.Milliseconds(), "capturedStdoutBytes": len(result.Stdout), "capturedStderrBytes": len(result.Stderr)}
}

func isolationDiagnostics(result UVResult, options resolvedRunOptions, operation string, cause error) map[string]any {
	details := runnerFailureDetails(result, options, "process_isolation_failed")
	details["operation"] = operation
	var errno syscall.Errno
	if errors.As(cause, &errno) {
		details["windowsError"] = uint64(errno)
	}
	return details
}

func networkExecutionDiagnostics(result UVResult, cause error, options RunOptions) map[string]any {
	details := executionDiagnostics(result, cause, "python_install")
	details["pythonVersion"], details["branch"], details["commit"] = options.PythonVersion, options.Branch, options.Commit
	return details
}

func pythonInstallFailure(spec PythonSpec, result UVResult, cause error) error {
	err := pythonError(protocol.CodePythonInstallFailed, protocol.StagePythonInstall, "Python 安装失败",
		map[string]any{"pythonVersion": spec.Version.String(), "exitCode": result.ExitCode}, cause)
	if failure, ok := err.(*Error); ok && failure.Code() == protocol.CodePythonInstallFailed {
		failure.diagnostics = executionDiagnostics(result, cause, "python_install")
		failure.diagnostics["pythonVersion"] = spec.Version.String()
		failure.diagnostics["sourceKind"] = mirror.KindPython.String()
		failure.diagnostics["attemptCount"] = 1
	}
	return err
}

func networkAttemptDiagnostics(reports []mirror.AttemptReport) []map[string]any {
	// 留出字段预算给最后一次执行的分类与计数，不让长尝试列表挤掉关键事实。
	if len(reports) > maxNetworkDiagnosticAttempts {
		reports = reports[len(reports)-maxNetworkDiagnosticAttempts:]
	}
	details := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		details = append(details, map[string]any{"source": report.SourceKey, "sourceTry": report.SourceTry,
			"globalTry": report.GlobalTry, "outcome": report.Outcome.String(), "durationMs": report.Duration.Milliseconds()})
	}
	return details
}
