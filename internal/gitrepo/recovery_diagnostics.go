package gitrepo

// recoveryDiagnosticError 保存恢复判定时已经观察的目录事实，不参与协议错误选择。
type recoveryDiagnosticError struct {
	cause    error
	snapshot recoveryDiagnostics
}

type recoveryDiagnostics struct {
	targetVersion string
	targetCommit  string
	repository    recoveryDirectoryDiagnostic
	update        recoveryDirectoryDiagnostic
	previous      recoveryDirectoryDiagnostic
}

type recoveryDirectoryDiagnostic struct {
	state    string
	identity repositoryIdentity
}

func (e *recoveryDiagnosticError) Error() string { return e.cause.Error() }
func (e *recoveryDiagnosticError) Unwrap() error { return e.cause }

// DiagnosticDetails 返回检查时的快照，不访问磁盘或暴露目录路径。
func (e *recoveryDiagnosticError) DiagnosticDetails() map[string]any {
	details := map[string]any{"directories": []map[string]any{
		e.snapshot.repository.details("repo"),
		e.snapshot.update.details("update"),
		e.snapshot.previous.details("previous"),
	}}
	if e.snapshot.targetVersion != "" {
		details["expectedVersion"] = e.snapshot.targetVersion
	}
	if e.snapshot.targetCommit != "" {
		details["commit"] = e.snapshot.targetCommit
	}
	return details
}

func (p recoveryDirectoryDiagnostic) details(role string) map[string]any {
	status := p.state
	if status == "" {
		status = "not_observed"
	}
	details := map[string]any{"component": role, "state": status}
	if status == "valid" {
		details["version"] = p.identity.version
		details["commit"] = p.identity.commit
	}
	return details
}

func describeRecoveryDirectory(path recoveryPath, err error) recoveryDirectoryDiagnostic {
	if err != nil {
		return recoveryDirectoryDiagnostic{state: "unknown"}
	}
	switch path.kind {
	case recoveryPathMissing:
		return recoveryDirectoryDiagnostic{state: "missing"}
	case recoveryPathIncomplete:
		return recoveryDirectoryDiagnostic{state: "incomplete"}
	case recoveryPathValid:
		return recoveryDirectoryDiagnostic{state: "valid", identity: path.identity}
	default:
		return recoveryDirectoryDiagnostic{state: "unknown"}
	}
}
