package protocol

import (
	"fmt"
	"slices"
)

// Code 是稳定且机器可读的协议结果码或错误码。
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

// ExitCode 是进程结果的粗粒度分类。
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

// Remediation 是向协议消费方公开的稳定处置动作标识。
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

// ErrorDefinition 包含错误码对应的稳定行为四元组。
type ErrorDefinition struct {
	Code        Code
	ExitCode    int
	Retryable   bool
	Remediation []Remediation
}

var errorDefinitions = []ErrorDefinition{
	{Code: CodeInvalidArgument, ExitCode: ExitCodeInvalidArgument, Retryable: false, Remediation: []Remediation{RemediationRunDoctor}},
	{Code: CodeInvalidControlCommand, ExitCode: ExitCodeSuccess, Retryable: false, Remediation: []Remediation{RemediationUpdateDesktop}},
	{Code: CodeInvalidVersion, ExitCode: ExitCodeInvalidArgument, Retryable: false, Remediation: []Remediation{RemediationSelectVersion}},
	{Code: CodeUnsupportedMode, ExitCode: ExitCodeInvalidArgument, Retryable: false, Remediation: []Remediation{RemediationUpdateDesktop}},
	{Code: CodeProtocolMismatch, ExitCode: ExitCodeProtocolMismatch, Retryable: false, Remediation: []Remediation{RemediationUpdateDesktop}},
	{Code: CodeOperationCancelled, ExitCode: ExitCodeOperationCancelled, Retryable: true, Remediation: []Remediation{RemediationRetry}},
	{Code: CodeOutputWriteFailed, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationOpenLog, RemediationContactSupport}},
	{Code: CodePathOutsideManagedRoot, ExitCode: ExitCodeOperationConflict, Retryable: false, Remediation: []Remediation{RemediationRunDoctor}},
	{Code: CodeUnsafeReparsePoint, ExitCode: ExitCodeOperationConflict, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodeDirectoryOccupied, ExitCode: ExitCodeOperationConflict, Retryable: true, Remediation: []Remediation{RemediationRetry}},
	{Code: CodeMutationInProgress, ExitCode: ExitCodeOperationConflict, Retryable: true, Remediation: []Remediation{RemediationRetry}},
	{Code: CodeBackendAlreadyRunning, ExitCode: ExitCodeOperationConflict, Retryable: false, Remediation: []Remediation{}},
	{Code: CodeBackendStillRunning, ExitCode: ExitCodeOperationConflict, Retryable: true, Remediation: []Remediation{RemediationStopBackend}},
	{Code: CodeStateWriteFailed, ExitCode: ExitCodeOperationConflict, Retryable: true, Remediation: []Remediation{RemediationRetry, RemediationRunDoctor}},
	{Code: CodeUpdateStateAmbiguous, ExitCode: ExitCodeOperationConflict, Retryable: false, Remediation: []Remediation{RemediationRunDoctor, RemediationContactSupport}},
	{Code: CodeNetworkUnavailable, ExitCode: ExitCodeNetworkFailure, Retryable: true, Remediation: []Remediation{RemediationRetry, RemediationRunDoctor}},
	{Code: CodeMirrorExhausted, ExitCode: ExitCodeNetworkFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror}},
	{Code: CodeGitBranchNotFound, ExitCode: ExitCodeGitFailure, Retryable: false, Remediation: []Remediation{RemediationSelectVersion}},
	{Code: CodeGitRemoteResolveFailed, ExitCode: ExitCodeNetworkFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror}},
	{Code: CodeGitCloneFailed, ExitCode: ExitCodeNetworkFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror}},
	{Code: CodeGitRepositoryInvalid, ExitCode: ExitCodeGitFailure, Retryable: true, Remediation: []Remediation{RemediationRetrySync}},
	{Code: CodeGitVersionMismatch, ExitCode: ExitCodeGitFailure, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodeGitRepoSwapFailed, ExitCode: ExitCodeGitFailure, Retryable: true, Remediation: []Remediation{RemediationRetry, RemediationRunDoctor}},
	{Code: CodeGitRepoCleanupFailed, ExitCode: ExitCodeGitFailure, Retryable: true, Remediation: []Remediation{RemediationCleanup, RemediationOpenLog}},
	{Code: CodeUVDownloadFailed, ExitCode: ExitCodeNetworkFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror}},
	{Code: CodeUVChecksumMismatch, ExitCode: ExitCodeGitFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror, RemediationContactSupport}},
	{Code: CodeUVVersionMismatch, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationUpdateDesktop}},
	{Code: CodeUVExecFailed, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{Code: CodePythonVersionFileMissing, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodePythonVersionInvalid, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodePythonVersionUnsupported, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationUpdateDesktop}},
	{Code: CodePythonVersionIncompatible, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodePythonInstallFailed, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRetryOtherMirror, RemediationOpenLog}},
	{Code: CodePythonVersionMismatch, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRebuildEnvironment}},
	{Code: CodeLockfileMissing, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodeLockfileOutdated, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationContactSupport}},
	{Code: CodeDependencySyncFailed, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRetrySync, RemediationRebuildEnvironment, RemediationOpenLog}},
	{Code: CodeEnvironmentBroken, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRetrySync, RemediationRebuildEnvironment}},
	{Code: CodeEnvironmentRebuildFailed, ExitCode: ExitCodeEnvironmentFailure, Retryable: true, Remediation: []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{Code: CodeBackendEntryNotFound, ExitCode: ExitCodePreconditionFailed, Retryable: false, Remediation: []Remediation{RemediationRetrySync, RemediationContactSupport}},
	{Code: CodeBackendSpawnFailed, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRunDoctor, RemediationOpenLog}},
	{Code: CodeBackendExitedBeforeReady, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{Code: CodeBackendHealthTimeout, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{Code: CodeBackendHealthInvalid, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{Code: CodeBackendIdentityMismatch, ExitCode: ExitCodeBackendFailure, Retryable: false, Remediation: []Remediation{RemediationRetrySync, RemediationContactSupport}},
	{Code: CodeBackendExitedUnexpectedly, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRestartBackend, RemediationOpenLog}},
	{Code: CodeBackendRestartFailed, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRestartBackend, RemediationRebuildEnvironment}},
	{Code: CodeBackendShutdownFailed, ExitCode: ExitCodeBackendFailure, Retryable: true, Remediation: []Remediation{RemediationRetry, RemediationOpenLog}},
	{Code: CodeBackendForceTerminated, ExitCode: ExitCodeSuccess, Retryable: false, Remediation: []Remediation{RemediationOpenLog}},
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

// AllErrorDefinitions 按文档顺序返回全部稳定错误定义的防御性副本。
func AllErrorDefinitions() []ErrorDefinition {
	definitions := make([]ErrorDefinition, len(errorDefinitions))
	for i, definition := range errorDefinitions {
		definitions[i] = cloneErrorDefinition(definition)
	}
	return definitions
}

// LookupErrorDefinition 返回 code 对应的稳定定义及是否存在。
func LookupErrorDefinition(code Code) (ErrorDefinition, bool) {
	definition, ok := errorDefinitionByCode[code]
	if !ok {
		return ErrorDefinition{}, false
	}
	return cloneErrorDefinition(definition), true
}

// AllRemediations 返回全部稳定处置动作的防御性副本。
func AllRemediations() []Remediation {
	return append([]Remediation(nil), remediations...)
}

// IsKnownCode 报告 value 是否为 OK 或错误定义中的稳定错误码。
func IsKnownCode(value Code) bool {
	if value == CodeOK {
		return true
	}
	_, ok := errorDefinitionByCode[value]
	return ok
}

// IsKnownRemediation 报告 value 是否为稳定处置动作。
func IsKnownRemediation(value Remediation) bool {
	return slices.Contains(remediations, value)
}

// NewErrorEvent 根据 code 的稳定定义构造 error 事件。
func NewErrorEvent(code Code, stage Stage, message string, details map[string]any) (ErrorEvent, error) {
	definition, ok := LookupErrorDefinition(code)
	if !ok {
		return ErrorEvent{}, fmt.Errorf("unknown protocol error code %q", code)
	}
	if definition.ExitCode == ExitCodeSuccess {
		return ErrorEvent{}, fmt.Errorf("protocol code %q is warning-only", code)
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

// NewWarningEvent 根据 code 的稳定定义构造 warning 事件。
func NewWarningEvent(code Code, stage Stage, message string, details map[string]any) (WarningEvent, error) {
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

// NewFailureResult 构造失败 result，并复述主错误的稳定四元组。
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

// NewSuccessResult 构造成功 result。
func NewSuccessResult(stage Stage, status string, message string, details map[string]any) ResultEvent {
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
