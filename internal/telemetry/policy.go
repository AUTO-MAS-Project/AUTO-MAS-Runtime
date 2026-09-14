package telemetry

import "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"

// failureReportability 显式记录每个失败码是否值得发送；新码缺席时默认拒绝。
var failureReportability = map[protocol.Code]bool{
	protocol.CodeInvalidArgument:           false,
	protocol.CodeInvalidVersion:            false,
	protocol.CodeUnsupportedMode:           true,
	protocol.CodeProtocolMismatch:          true,
	protocol.CodeOperationCancelled:        false,
	protocol.CodeOutputWriteFailed:         true,
	protocol.CodeInternalError:             true,
	protocol.CodePathOutsideManagedRoot:    true,
	protocol.CodeUnsafeReparsePoint:        true,
	protocol.CodeDirectoryOccupied:         true,
	protocol.CodeMutationInProgress:        false,
	protocol.CodeBackendAlreadyRunning:     false,
	protocol.CodeBackendStillRunning:       false,
	protocol.CodeMutexOperationFailed:      true,
	protocol.CodeStateWriteFailed:          true,
	protocol.CodeUpdateStateAmbiguous:      true,
	protocol.CodeNetworkUnavailable:        true,
	protocol.CodeMirrorExhausted:           true,
	protocol.CodeGitBranchNotFound:         true,
	protocol.CodeGitRemoteResolveFailed:    true,
	protocol.CodeGitCloneFailed:            true,
	protocol.CodeGitRepositoryInvalid:      true,
	protocol.CodeGitVersionMismatch:        true,
	protocol.CodeGitRepoSwapFailed:         true,
	protocol.CodeGitRepoCleanupFailed:      true,
	protocol.CodeUVDownloadFailed:          true,
	protocol.CodeUVChecksumMismatch:        true,
	protocol.CodeUVVersionMismatch:         true,
	protocol.CodeUVExecFailed:              true,
	protocol.CodePythonVersionFileMissing:  true,
	protocol.CodePythonVersionInvalid:      true,
	protocol.CodePythonVersionUnsupported:  true,
	protocol.CodePythonVersionIncompatible: true,
	protocol.CodePythonInstallFailed:       true,
	protocol.CodePythonVersionMismatch:     true,
	protocol.CodeLockfileMissing:           true,
	protocol.CodeLockfileOutdated:          true,
	protocol.CodeDependencySyncFailed:      true,
	protocol.CodeEnvironmentBroken:         true,
	protocol.CodeEnvironmentRebuildFailed:  true,
	protocol.CodeBackendEntryNotFound:      true,
	protocol.CodeBackendSpawnFailed:        true,
	protocol.CodeBackendExitedBeforeReady:  true,
	protocol.CodeBackendHealthTimeout:      true,
	protocol.CodeBackendHealthInvalid:      true,
	protocol.CodeBackendIdentityMismatch:   true,
	protocol.CodeBackendExitedUnexpectedly: true,
	protocol.CodeBackendRestartFailed:      true,
	protocol.CodeBackendShutdownFailed:     true,
}

// IsReportableFailure 报告稳定失败码是否经过价值评审并允许发送。
func IsReportableFailure(code protocol.Code) bool {
	reportable, explicitlyClassified := failureReportability[code]
	if !explicitlyClassified || !reportable {
		return false
	}
	definition, known := protocol.LookupErrorDefinition(code)
	return known && definition.ExitCode != protocol.ExitCodeSuccess
}
