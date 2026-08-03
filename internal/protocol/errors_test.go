package protocol_test

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestErrorDefinitionsMatchArchitectureDocument(t *testing.T) {
	t.Parallel()

	documented := documentedErrorDefinitions(t)
	implemented := protocol.AllErrorDefinitions()

	if !reflect.DeepEqual(implemented, documented) {
		t.Fatalf("implemented error definitions do not match doc/架构设计.md\nimplemented: %#v\ndocumented: %#v", implemented, documented)
	}

	declaredCodes := []protocol.Code{
		protocol.CodeInvalidArgument,
		protocol.CodeInvalidControlCommand,
		protocol.CodeInvalidVersion,
		protocol.CodeUnsupportedMode,
		protocol.CodeProtocolMismatch,
		protocol.CodeOperationCancelled,
		protocol.CodeOutputWriteFailed,
		protocol.CodeInternalError,
		protocol.CodePathOutsideManagedRoot,
		protocol.CodeUnsafeReparsePoint,
		protocol.CodeDirectoryOccupied,
		protocol.CodeMutationInProgress,
		protocol.CodeBackendAlreadyRunning,
		protocol.CodeBackendStillRunning,
		protocol.CodeMutexOperationFailed,
		protocol.CodeStateWriteFailed,
		protocol.CodeUpdateStateAmbiguous,
		protocol.CodeNetworkUnavailable,
		protocol.CodeMirrorExhausted,
		protocol.CodeGitBranchNotFound,
		protocol.CodeGitRemoteResolveFailed,
		protocol.CodeGitCloneFailed,
		protocol.CodeGitRepositoryInvalid,
		protocol.CodeGitVersionMismatch,
		protocol.CodeGitRepoSwapFailed,
		protocol.CodeGitRepoCleanupFailed,
		protocol.CodeUVDownloadFailed,
		protocol.CodeUVChecksumMismatch,
		protocol.CodeUVVersionMismatch,
		protocol.CodeUVExecFailed,
		protocol.CodePythonVersionFileMissing,
		protocol.CodePythonVersionInvalid,
		protocol.CodePythonVersionUnsupported,
		protocol.CodePythonVersionIncompatible,
		protocol.CodePythonInstallFailed,
		protocol.CodePythonVersionMismatch,
		protocol.CodeLockfileMissing,
		protocol.CodeLockfileOutdated,
		protocol.CodeDependencySyncFailed,
		protocol.CodeEnvironmentBroken,
		protocol.CodeEnvironmentRebuildFailed,
		protocol.CodeBackendEntryNotFound,
		protocol.CodeBackendSpawnFailed,
		protocol.CodeBackendExitedBeforeReady,
		protocol.CodeBackendHealthTimeout,
		protocol.CodeBackendHealthInvalid,
		protocol.CodeBackendIdentityMismatch,
		protocol.CodeBackendExitedUnexpectedly,
		protocol.CodeBackendRestartFailed,
		protocol.CodeBackendShutdownFailed,
		protocol.CodeBackendForceTerminated,
	}

	if len(declaredCodes) != len(documented) {
		t.Fatalf("declared code count = %d, documented count = %d", len(declaredCodes), len(documented))
	}
	for i, code := range declaredCodes {
		if code != documented[i].Code {
			t.Errorf("declared code %d = %q, documented = %q", i, code, documented[i].Code)
		}

		definition, ok := protocol.LookupErrorDefinition(code)
		if !ok {
			t.Errorf("LookupErrorDefinition(%q) not found", code)
			continue
		}
		if !reflect.DeepEqual(definition, documented[i]) {
			t.Errorf("LookupErrorDefinition(%q) = %#v, want %#v", code, definition, documented[i])
		}
	}
}

func TestRemediationConstantsAreComplete(t *testing.T) {
	t.Parallel()

	implemented := protocol.AllRemediations()
	want := []protocol.Remediation{
		protocol.RemediationRetry,
		protocol.RemediationRetrySync,
		protocol.RemediationRetryOtherMirror,
		protocol.RemediationRebuildEnvironment,
		protocol.RemediationStopBackend,
		protocol.RemediationRestartBackend,
		protocol.RemediationSelectVersion,
		protocol.RemediationUpdateDesktop,
		protocol.RemediationRunDoctor,
		protocol.RemediationCleanup,
		protocol.RemediationOpenLog,
		protocol.RemediationContactSupport,
	}

	if !reflect.DeepEqual(implemented, want) {
		t.Fatalf("AllRemediations() = %#v, want %#v", implemented, want)
	}
}

func TestStableErrorValueQueries(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		code protocol.Code
		want bool
	}{
		{name: "success", code: protocol.CodeOK, want: true},
		{name: "defined error", code: protocol.CodeOperationCancelled, want: true},
		{name: "unknown", code: protocol.Code("FUTURE_UNKNOWN_CODE"), want: false},
	} {
		test := test
		t.Run("code "+test.name, func(t *testing.T) {
			t.Parallel()
			if got := protocol.IsKnownCode(test.code); got != test.want {
				t.Errorf("IsKnownCode(%q) = %t, want %t", test.code, got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name        string
		remediation protocol.Remediation
		want        bool
	}{
		{name: "defined", remediation: protocol.RemediationRetry, want: true},
		{name: "unknown", remediation: protocol.Remediation("future-action"), want: false},
	} {
		test := test
		t.Run("remediation "+test.name, func(t *testing.T) {
			t.Parallel()
			if got := protocol.IsKnownRemediation(test.remediation); got != test.want {
				t.Errorf(
					"IsKnownRemediation(%q) = %t, want %t",
					test.remediation,
					got,
					test.want,
				)
			}
		})
	}
}

func TestExitCodeConstantsAreStable(t *testing.T) {
	t.Parallel()

	got := []protocol.ExitCode{
		protocol.ExitCodeSuccess,
		protocol.ExitCodeInvalidArgument,
		protocol.ExitCodeProtocolMismatch,
		protocol.ExitCodePreconditionFailed,
		protocol.ExitCodeNetworkFailure,
		protocol.ExitCodeGitFailure,
		protocol.ExitCodeEnvironmentFailure,
		protocol.ExitCodeBackendFailure,
		protocol.ExitCodeOperationConflict,
		protocol.ExitCodeOperationCancelled,
	}
	want := []protocol.ExitCode{0, 2, 10, 20, 30, 40, 50, 60, 70, 130}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exit codes = %#v, want %#v", got, want)
	}
}

func TestDefinitionQueriesReturnDefensiveCopies(t *testing.T) {
	t.Parallel()

	all := protocol.AllErrorDefinitions()
	all[0].Remediation[0] = protocol.RemediationContactSupport
	fresh := protocol.AllErrorDefinitions()
	if fresh[0].Remediation[0] != protocol.RemediationRunDoctor {
		t.Fatalf("AllErrorDefinitions() exposed shared remediation storage: %#v", fresh[0])
	}

	lookup, ok := protocol.LookupErrorDefinition(protocol.CodeDependencySyncFailed)
	if !ok {
		t.Fatal("LookupErrorDefinition() not found")
	}
	lookup.Remediation[0] = protocol.RemediationContactSupport
	freshLookup, ok := protocol.LookupErrorDefinition(protocol.CodeDependencySyncFailed)
	if !ok {
		t.Fatal("second LookupErrorDefinition() not found")
	}
	if freshLookup.Remediation[0] != protocol.RemediationRetrySync {
		t.Fatalf("LookupErrorDefinition() exposed shared remediation storage: %#v", freshLookup)
	}

	remediations := protocol.AllRemediations()
	remediations[0] = protocol.RemediationContactSupport
	if freshRemediations := protocol.AllRemediations(); freshRemediations[0] != protocol.RemediationRetry {
		t.Fatalf("AllRemediations() exposed shared storage: %#v", freshRemediations)
	}
}

func TestEventConstructorsUseStableErrorDefinition(t *testing.T) {
	t.Parallel()

	primary, err := protocol.NewErrorEvent(
		protocol.CodeDependencySyncFailed,
		protocol.StageDependenciesSync,
		"Python 依赖安装失败",
		map[string]any{"exitCode": 1},
	)
	if err != nil {
		t.Fatalf("NewErrorEvent() error = %v", err)
	}
	if !primary.Retryable {
		t.Error("error.retryable = false, want true")
	}
	wantRemediation := []string{"retry-sync", "rebuild-environment", "open-log"}
	if !reflect.DeepEqual(primary.Remediation, wantRemediation) {
		t.Errorf("error.remediation = %#v, want %#v", primary.Remediation, wantRemediation)
	}

	result := protocol.NewFailureResult(
		primary,
		"environment_broken",
		"Python 依赖同步失败",
		map[string]any{"logPath": "logs/runtime/dependencies-20260728.log"},
	)
	if result.Success {
		t.Error("result.success = true, want false")
	}
	if result.Code != primary.Code ||
		result.Stage != primary.Stage ||
		result.Retryable != primary.Retryable ||
		!reflect.DeepEqual(result.Remediation, primary.Remediation) {
		t.Errorf("failure result does not repeat primary error tuple: result=%#v primary=%#v", result, primary)
	}

	warning, err := protocol.NewWarningEvent(
		protocol.CodeBackendForceTerminated,
		protocol.StageBackendShutdown,
		"后端已被强制终止",
		nil,
	)
	if err != nil {
		t.Fatalf("NewWarningEvent() error = %v", err)
	}
	if warning.Code != string(protocol.CodeBackendForceTerminated) ||
		warning.Retryable ||
		!reflect.DeepEqual(warning.Remediation, []string{"open-log"}) {
		t.Errorf("warning = %#v", warning)
	}

	success := protocol.NewSuccessResult(protocol.StageDoctor, "ready_to_start", "检查通过", nil)
	if !success.Success || success.Code != string(protocol.CodeOK) || success.Retryable {
		t.Errorf("success result = %#v", success)
	}
	if len(success.Remediation) != 0 {
		t.Errorf("success remediation = %#v, want empty", success.Remediation)
	}
}

func TestEventConstructorsRejectUnknownCode(t *testing.T) {
	t.Parallel()

	const unknown protocol.Code = "FUTURE_UNKNOWN_CODE"
	if _, err := protocol.NewErrorEvent(unknown, protocol.StageDoctor, "unknown", nil); err == nil {
		t.Error("NewErrorEvent() error = nil, want unknown-code error")
	}
	if _, err := protocol.NewWarningEvent(unknown, protocol.StageDoctor, "unknown", nil); err == nil {
		t.Error("NewWarningEvent() error = nil, want unknown-code error")
	}
	if _, ok := protocol.LookupErrorDefinition(unknown); ok {
		t.Error("LookupErrorDefinition() ok = true, want false")
	}
}

func TestNewErrorEventRejectsWarningOnlyCodes(t *testing.T) {
	t.Parallel()

	for _, code := range []protocol.Code{
		protocol.CodeInvalidControlCommand,
		protocol.CodeBackendForceTerminated,
	} {
		code := code
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			if _, err := protocol.NewErrorEvent(code, protocol.StageDoctor, "warning-only", nil); err == nil {
				t.Errorf("NewErrorEvent(%q) error = nil, want warning-only-code error", code)
			}
		})
	}
}

func documentedErrorDefinitions(t *testing.T) []protocol.ErrorDefinition {
	t.Helper()

	content := docSection(
		t,
		readDoc(t, "架构设计.md"),
		"### 错误码全集",
		"### 已确认决策",
	)

	rowPattern := regexp.MustCompile("^\\| `([A-Z][A-Z0-9_]*)` \\| ([0-9]+) \\| (是|否) \\| (.*) \\|$")
	remediationPattern := regexp.MustCompile("`([^`]+)`")
	var definitions []protocol.ErrorDefinition
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		match := rowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		exitCode, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("parse exit code in row %q: %v", line, err)
		}
		remediationMatches := remediationPattern.FindAllStringSubmatch(match[4], -1)
		remediations := make([]protocol.Remediation, 0, len(remediationMatches))
		for _, remediation := range remediationMatches {
			remediations = append(remediations, protocol.Remediation(remediation[1]))
		}
		definitions = append(definitions, protocol.ErrorDefinition{
			Code:        protocol.Code(match[1]),
			ExitCode:    exitCode,
			Retryable:   match[3] == "是",
			Remediation: remediations,
		})
	}
	if len(definitions) == 0 {
		t.Fatal("no error definitions parsed from architecture document")
	}
	return definitions
}
