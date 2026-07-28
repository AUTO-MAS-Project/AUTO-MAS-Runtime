package protocol

import "fmt"

// Code is a stable machine-readable protocol result or error code.
type Code string

const (
	CodeOK                        Code = "OK"
	CodeInvalidArgument           Code = "INVALID_ARGUMENT"
	CodeInvalidControlCommand     Code = "INVALID_CONTROL_COMMAND"
	CodeInvalidVersion            Code = "INVALID_VERSION"
	CodeUnsupportedMode           Code = "UNSUPPORTED_MODE"
	CodeProtocolMismatch          Code = "PROTOCOL_MISMATCH"
	CodeOperationCancelled        Code = "OPERATION_CANCELLED"
	CodeOutputWriteFailed         Code = "OUTPUT_WRITE_FAILED"
	CodePathOutsideManagedRoot    Code = "PATH_OUTSIDE_MANAGED_ROOT"
	CodeUnsafeReparsePoint        Code = "UNSAFE_REPARSE_POINT"
	CodeDirectoryOccupied         Code = "DIRECTORY_OCCUPIED"
	CodeMutationInProgress        Code = "MUTATION_IN_PROGRESS"
	CodeBackendAlreadyRunning     Code = "BACKEND_ALREADY_RUNNING"
	CodeBackendStillRunning       Code = "BACKEND_STILL_RUNNING"
	CodeStateWriteFailed          Code = "STATE_WRITE_FAILED"
	CodeUpdateStateAmbiguous      Code = "UPDATE_STATE_AMBIGUOUS"
	CodeNetworkUnavailable        Code = "NETWORK_UNAVAILABLE"
	CodeMirrorExhausted           Code = "MIRROR_EXHAUSTED"
	CodeGitBranchNotFound         Code = "GIT_BRANCH_NOT_FOUND"
	CodeGitRemoteResolveFailed    Code = "GIT_REMOTE_RESOLVE_FAILED"
	CodeGitCloneFailed            Code = "GIT_CLONE_FAILED"
	CodeGitRepositoryInvalid      Code = "GIT_REPOSITORY_INVALID"
	CodeGitVersionMismatch        Code = "GIT_VERSION_MISMATCH"
	CodeGitRepoSwapFailed         Code = "GIT_REPO_SWAP_FAILED"
	CodeGitRepoCleanupFailed      Code = "GIT_REPO_CLEANUP_FAILED"
	CodeUVDownloadFailed          Code = "UV_DOWNLOAD_FAILED"
	CodeUVChecksumMismatch        Code = "UV_CHECKSUM_MISMATCH"
	CodeUVVersionMismatch         Code = "UV_VERSION_MISMATCH"
	CodeUVExecFailed              Code = "UV_EXEC_FAILED"
	CodePythonVersionFileMissing  Code = "PYTHON_VERSION_FILE_MISSING"
	CodePythonVersionInvalid      Code = "PYTHON_VERSION_INVALID"
	CodePythonVersionUnsupported  Code = "PYTHON_VERSION_UNSUPPORTED"
	CodePythonVersionIncompatible Code = "PYTHON_VERSION_INCOMPATIBLE"
	CodePythonInstallFailed       Code = "PYTHON_INSTALL_FAILED"
	CodePythonVersionMismatch     Code = "PYTHON_VERSION_MISMATCH"
	CodeLockfileMissing           Code = "LOCKFILE_MISSING"
	CodeLockfileOutdated          Code = "LOCKFILE_OUTDATED"
	CodeDependencySyncFailed      Code = "DEPENDENCY_SYNC_FAILED"
	CodeEnvironmentBroken         Code = "ENVIRONMENT_BROKEN"
	CodeEnvironmentRebuildFailed  Code = "ENVIRONMENT_REBUILD_FAILED"
	CodeBackendEntryNotFound      Code = "BACKEND_ENTRY_NOT_FOUND"
	CodeBackendSpawnFailed        Code = "BACKEND_SPAWN_FAILED"
	CodeBackendExitedBeforeReady  Code = "BACKEND_EXITED_BEFORE_READY"
	CodeBackendHealthTimeout      Code = "BACKEND_HEALTH_TIMEOUT"
	CodeBackendHealthInvalid      Code = "BACKEND_HEALTH_INVALID"
	CodeBackendIdentityMismatch   Code = "BACKEND_IDENTITY_MISMATCH"
	CodeBackendExitedUnexpectedly Code = "BACKEND_EXITED_UNEXPECTEDLY"
	CodeBackendRestartFailed      Code = "BACKEND_RESTART_FAILED"
	CodeBackendShutdownFailed     Code = "BACKEND_SHUTDOWN_FAILED"
	CodeBackendForceTerminated    Code = "BACKEND_FORCE_TERMINATED"
)

// ExitCode is a coarse process outcome classification.
type ExitCode = int

const (
	ExitCodeSuccess            ExitCode = 0
	ExitCodeInvalidArgument    ExitCode = 2
	ExitCodeProtocolMismatch   ExitCode = 10
	ExitCodePreconditionFailed ExitCode = 20
	ExitCodeNetworkFailure     ExitCode = 30
	ExitCodeGitFailure         ExitCode = 40
	ExitCodeEnvironmentFailure ExitCode = 50
	ExitCodeBackendFailure     ExitCode = 60
	ExitCodeOperationConflict  ExitCode = 70
	ExitCodeOperationCancelled ExitCode = 130
)

// Remediation is a stable action identifier exposed to protocol consumers.
type Remediation string

const (
	RemediationRetry              Remediation = "retry"
	RemediationRetrySync          Remediation = "retry-sync"
	RemediationRetryOtherMirror   Remediation = "retry-other-mirror"
	RemediationRebuildEnvironment Remediation = "rebuild-environment"
	RemediationStopBackend        Remediation = "stop-backend"
	RemediationRestartBackend     Remediation = "restart-backend"
	RemediationSelectVersion      Remediation = "select-version"
	RemediationUpdateDesktop      Remediation = "update-desktop"
	RemediationRunDoctor          Remediation = "run-doctor"
	RemediationCleanup            Remediation = "cleanup"
	RemediationOpenLog            Remediation = "open-log"
	RemediationContactSupport     Remediation = "contact-support"
)

// ErrorDefinition contains the stable behavior associated with an error code.
type ErrorDefinition struct {
	Code        Code
	ExitCode    int
	Retryable   bool
	Remediation []Remediation
}

var errorDefinitions = []ErrorDefinition{
	{CodeInvalidArgument, ExitCodeInvalidArgument, false, []Remediation{RemediationRunDoctor}},
	{CodeInvalidControlCommand, ExitCodeSuccess, false, []Remediation{RemediationUpdateDesktop}},
	{CodeInvalidVersion, ExitCodeInvalidArgument, false, []Remediation{RemediationSelectVersion}},
	{CodeUnsupportedMode, ExitCodeInvalidArgument, false, []Remediation{RemediationUpdateDesktop}},
	{CodeProtocolMismatch, ExitCodeProtocolMismatch, false, []Remediation{RemediationUpdateDesktop}},
	{CodeOperationCancelled, ExitCodeOperationCancelled, true, []Remediation{RemediationRetry}},
	{CodeOutputWriteFailed, ExitCodePreconditionFailed, false, []Remediation{RemediationOpenLog, RemediationContactSupport}},
	{CodePathOutsideManagedRoot, ExitCodeOperationConflict, false, []Remediation{RemediationRunDoctor}},
	{CodeUnsafeReparsePoint, ExitCodeOperationConflict, false, []Remediation{RemediationContactSupport}},
	{CodeDirectoryOccupied, ExitCodeOperationConflict, true, []Remediation{RemediationRetry}},
	{CodeMutationInProgress, ExitCodeOperationConflict, true, []Remediation{RemediationRetry}},
	{CodeBackendAlreadyRunning, ExitCodeOperationConflict, false, []Remediation{}},
	{CodeBackendStillRunning, ExitCodeOperationConflict, true, []Remediation{RemediationStopBackend}},
	{CodeStateWriteFailed, ExitCodeOperationConflict, true, []Remediation{RemediationRetry, RemediationRunDoctor}},
	{CodeUpdateStateAmbiguous, ExitCodeOperationConflict, false, []Remediation{RemediationRunDoctor, RemediationContactSupport}},
	{CodeNetworkUnavailable, ExitCodeNetworkFailure, true, []Remediation{RemediationRetry, RemediationRunDoctor}},
	{CodeMirrorExhausted, ExitCodeNetworkFailure, true, []Remediation{RemediationRetryOtherMirror}},
	{CodeGitBranchNotFound, ExitCodeGitFailure, false, []Remediation{RemediationSelectVersion}},
	{CodeGitRemoteResolveFailed, ExitCodeNetworkFailure, true, []Remediation{RemediationRetryOtherMirror}},
	{CodeGitCloneFailed, ExitCodeNetworkFailure, true, []Remediation{RemediationRetryOtherMirror}},
	{CodeGitRepositoryInvalid, ExitCodeGitFailure, true, []Remediation{RemediationRetrySync}},
	{CodeGitVersionMismatch, ExitCodeGitFailure, false, []Remediation{RemediationContactSupport}},
	{CodeGitRepoSwapFailed, ExitCodeGitFailure, true, []Remediation{RemediationRetry, RemediationRunDoctor}},
	{CodeGitRepoCleanupFailed, ExitCodeGitFailure, true, []Remediation{RemediationCleanup, RemediationOpenLog}},
	{CodeUVDownloadFailed, ExitCodeNetworkFailure, true, []Remediation{RemediationRetryOtherMirror}},
	{CodeUVChecksumMismatch, ExitCodeGitFailure, true, []Remediation{RemediationRetryOtherMirror, RemediationContactSupport}},
	{CodeUVVersionMismatch, ExitCodePreconditionFailed, false, []Remediation{RemediationUpdateDesktop}},
	{CodeUVExecFailed, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{CodePythonVersionFileMissing, ExitCodePreconditionFailed, false, []Remediation{RemediationContactSupport}},
	{CodePythonVersionInvalid, ExitCodePreconditionFailed, false, []Remediation{RemediationContactSupport}},
	{CodePythonVersionUnsupported, ExitCodePreconditionFailed, false, []Remediation{RemediationUpdateDesktop}},
	{CodePythonVersionIncompatible, ExitCodePreconditionFailed, false, []Remediation{RemediationContactSupport}},
	{CodePythonInstallFailed, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRetryOtherMirror, RemediationOpenLog}},
	{CodePythonVersionMismatch, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRebuildEnvironment}},
	{CodeLockfileMissing, ExitCodePreconditionFailed, false, []Remediation{RemediationContactSupport}},
	{CodeLockfileOutdated, ExitCodePreconditionFailed, false, []Remediation{RemediationContactSupport}},
	{CodeDependencySyncFailed, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRetrySync, RemediationRebuildEnvironment, RemediationOpenLog}},
	{CodeEnvironmentBroken, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRetrySync, RemediationRebuildEnvironment}},
	{CodeEnvironmentRebuildFailed, ExitCodeEnvironmentFailure, true, []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{CodeBackendEntryNotFound, ExitCodePreconditionFailed, false, []Remediation{RemediationRetrySync, RemediationContactSupport}},
	{CodeBackendSpawnFailed, ExitCodeBackendFailure, true, []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{CodeBackendExitedBeforeReady, ExitCodeBackendFailure, true, []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{CodeBackendHealthTimeout, ExitCodeBackendFailure, true, []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{CodeBackendHealthInvalid, ExitCodeBackendFailure, true, []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{CodeBackendIdentityMismatch, ExitCodeBackendFailure, false, []Remediation{RemediationRetrySync, RemediationContactSupport}},
	{CodeBackendExitedUnexpectedly, ExitCodeBackendFailure, true, []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{CodeBackendRestartFailed, ExitCodeBackendFailure, true, []Remediation{RemediationRestartBackend, RemediationRebuildEnvironment}},
	{CodeBackendShutdownFailed, ExitCodeBackendFailure, true, []Remediation{RemediationRetry, RemediationOpenLog}},
	{CodeBackendForceTerminated, ExitCodeSuccess, false, []Remediation{RemediationOpenLog}},
}

var errorDefinitionByCode = buildErrorDefinitionIndex(errorDefinitions)

var remediations = []Remediation{
	RemediationRetry,
	RemediationRetrySync,
	RemediationRetryOtherMirror,
	RemediationRebuildEnvironment,
	RemediationStopBackend,
	RemediationRestartBackend,
	RemediationSelectVersion,
	RemediationUpdateDesktop,
	RemediationRunDoctor,
	RemediationCleanup,
	RemediationOpenLog,
	RemediationContactSupport,
}

// AllErrorDefinitions returns every stable error definition in document order.
func AllErrorDefinitions() []ErrorDefinition {
	definitions := make([]ErrorDefinition, len(errorDefinitions))
	for i, definition := range errorDefinitions {
		definitions[i] = cloneErrorDefinition(definition)
	}
	return definitions
}

// LookupErrorDefinition returns the stable definition for code.
func LookupErrorDefinition(code Code) (ErrorDefinition, bool) {
	definition, ok := errorDefinitionByCode[code]
	if !ok {
		return ErrorDefinition{}, false
	}
	return cloneErrorDefinition(definition), true
}

// AllRemediations returns every stable remediation action.
func AllRemediations() []Remediation {
	return append([]Remediation(nil), remediations...)
}

// NewErrorEvent constructs an error event from the stable definition for code.
func NewErrorEvent(code Code, stage, message string, details map[string]any) (ErrorEvent, error) {
	definition, ok := LookupErrorDefinition(code)
	if !ok {
		return ErrorEvent{}, fmt.Errorf("unknown protocol error code %q", code)
	}
	return ErrorEvent{
		Code:        string(code),
		Stage:       stage,
		Message:     message,
		Retryable:   definition.Retryable,
		Remediation: remediationStrings(definition.Remediation),
		Details:     details,
	}, nil
}

// NewWarningEvent constructs a warning event from the stable definition for code.
func NewWarningEvent(code Code, stage, message string, details map[string]any) (WarningEvent, error) {
	definition, ok := LookupErrorDefinition(code)
	if !ok {
		return WarningEvent{}, fmt.Errorf("unknown protocol error code %q", code)
	}
	return WarningEvent{
		Code:        string(code),
		Stage:       stage,
		Message:     message,
		Retryable:   definition.Retryable,
		Remediation: remediationStrings(definition.Remediation),
		Details:     details,
	}, nil
}

// NewFailureResult constructs a failed result that repeats the primary error tuple.
func NewFailureResult(primary ErrorEvent, status, message string, details map[string]any) ResultEvent {
	return ResultEvent{
		Success:     false,
		Code:        primary.Code,
		Stage:       primary.Stage,
		Status:      status,
		Message:     message,
		Retryable:   primary.Retryable,
		Remediation: append([]string(nil), primary.Remediation...),
		Details:     details,
	}
}

// NewSuccessResult constructs a successful result.
func NewSuccessResult(stage, status, message string, details map[string]any) ResultEvent {
	return ResultEvent{
		Success:     true,
		Code:        string(CodeOK),
		Stage:       stage,
		Status:      status,
		Message:     message,
		Retryable:   false,
		Remediation: []string{},
		Details:     details,
	}
}

func buildErrorDefinitionIndex(definitions []ErrorDefinition) map[Code]ErrorDefinition {
	index := make(map[Code]ErrorDefinition, len(definitions))
	for _, definition := range definitions {
		index[definition.Code] = definition
	}
	return index
}

func cloneErrorDefinition(definition ErrorDefinition) ErrorDefinition {
	definition.Remediation = append([]Remediation(nil), definition.Remediation...)
	if definition.Remediation == nil {
		definition.Remediation = []Remediation{}
	}
	return definition
}

func remediationStrings(values []Remediation) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
