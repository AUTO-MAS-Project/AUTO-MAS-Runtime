package contracttest

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestContract_BusinessSchemas(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		target   int
		field    string
		value    any
		want     string
	}{
		{
			name:     "hello runtime version",
			terminal: TerminalSuccess,
			events:   successObjects,
			target:   0,
			field:    "runtimeVersion",
			value:    nil,
			want:     `hello field "runtimeVersion" must be a string`,
		},
		{
			name:     "hello command",
			terminal: TerminalSuccess,
			events:   successObjects,
			target:   0,
			field:    "command",
			value:    1,
			want:     `hello field "command" must be a string`,
		},
		{
			name:     "hello capability item",
			terminal: TerminalSuccess,
			events:   successObjects,
			target:   0,
			field:    "capabilities",
			value:    []any{1},
			want:     `hello field "capabilities" must be an array of strings`,
		},
		{
			name:     "progress stage",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "stage",
			value:    false,
			want:     `progress field "stage" must be a string`,
		},
		{
			name:     "progress status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "status",
			value:    1,
			want:     `progress field "status" must be a string`,
		},
		{
			name:     "progress message",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "message",
			value:    nil,
			want:     `progress field "message" must be a string`,
		},
		{
			name:     "progress current",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "current",
			value:    "one",
			want:     `progress field "current" must be an integer when present`,
		},
		{
			name:     "progress total",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "total",
			value:    1.5,
			want:     `progress field "total" must be an integer when present`,
		},
		{
			name:     "progress percent",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			target:   1,
			field:    "percent",
			value:    "half",
			want:     `progress field "percent" must be a number when present`,
		},
		{
			name:     "state stage",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			target:   1,
			field:    "stage",
			value:    true,
			want:     `state field "stage" must be a string`,
		},
		{
			name:     "state status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			target:   1,
			field:    "status",
			value:    nil,
			want:     `state field "status" must be a string`,
		},
		{
			name:     "state message",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			target:   1,
			field:    "message",
			value:    nil,
			want:     `state field "message" must be a string`,
		},
		{
			name:     "state details",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			target:   1,
			field:    "details",
			value:    []any{},
			want:     `state field "details" must be an object`,
		},
		{
			name:     "log source",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(logObjectForContract(2)) },
			target:   1,
			field:    "source",
			value:    1,
			want:     `log field "source" must be a string`,
		},
		{
			name:     "log stream",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(logObjectForContract(2)) },
			target:   1,
			field:    "stream",
			value:    nil,
			want:     `log field "stream" must be a string`,
		},
		{
			name:     "log message",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(logObjectForContract(2)) },
			target:   1,
			field:    "message",
			value:    false,
			want:     `log field "message" must be a string`,
		},
		{
			name:     "warning message",
			terminal: TerminalSuccess,
			events:   stableWarningObjectsForContract,
			target:   1,
			field:    "message",
			value:    nil,
			want:     `warning field "message" must be a string`,
		},
		{
			name:     "error message",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			target:   1,
			field:    "message",
			value:    nil,
			want:     `error field "message" must be a string`,
		},
		{
			name:     "result message",
			terminal: TerminalSuccess,
			events:   successObjects,
			target:   1,
			field:    "message",
			value:    nil,
			want:     `result field "message" must be a string`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			events := test.events()
			if test.value == nil {
				delete(events[test.target], test.field)
			} else {
				events[test.target][test.field] = test.value
			}
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}
}

func TestContract_ProgressOptionalFieldsMayBeAbsent(t *testing.T) {
	t.Parallel()

	progress := progressObjectForContract(2)
	for _, field := range []string{"current", "total", "percent"} {
		delete(progress, field)
	}
	_, issues := inspect(
		testCommand,
		TerminalSuccess,
		encodeTranscript(t, successWithMiddle(progress)),
	)
	requireNoIssues(t, issues)
}

func TestContract_RejectsNullContainers(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		mutate   func([]map[string]any)
		want     string
	}{
		{
			name:     "hello capabilities",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				events[0]["capabilities"] = nil
			},
			want: `hello field "capabilities" must be an array of strings`,
		},
		{
			name:     "state details",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			mutate: func(events []map[string]any) {
				events[1]["details"] = nil
			},
			want: `state field "details" must be an object`,
		},
		{
			name:     "warning remediation",
			terminal: TerminalSuccess,
			events:   stableWarningObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["remediation"] = nil
				warningSummaryForContract(events)["remediation"] = nil
			},
			want: `warning field "remediation" must be an array of strings`,
		},
		{
			name:     "error details",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["details"] = nil
			},
			want: `error field "details" must be an object`,
		},
		{
			name:     "result details",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				lastObject(events)["details"] = nil
			},
			want: `result field "details" must be an object`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			events := test.events()
			test.mutate(events)
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}
}

func TestContract_StableValueSets(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		mutate   func([]map[string]any)
		want     string
	}{
		{
			name:     "unknown code",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["code"] = "FUTURE_UNKNOWN_CODE"
				lastObject(events)["code"] = "FUTURE_UNKNOWN_CODE"
			},
			want: `error code "FUTURE_UNKNOWN_CODE" is not stable`,
		},
		{
			name:     "unknown stage",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				lastObject(events)["stage"] = "future.stage"
			},
			want: `result stage "future.stage" is not stable`,
		},
		{
			name:     "unknown remediation",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["remediation"] = []string{"future-action"}
				lastObject(events)["remediation"] = []string{"future-action"}
			},
			want: `error remediation "future-action" is not stable`,
		},
		{
			name:     "unknown progress status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			mutate: func(events []map[string]any) {
				events[1]["status"] = "future"
			},
			want: `progress status "future" is not stable`,
		},
		{
			name:     "unknown state status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			mutate: func(events []map[string]any) {
				events[1]["status"] = "future"
			},
			want: `state status "future" is not stable`,
		},
		{
			name:     "unknown capability",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				events[0]["capabilities"] = []string{"future.capability"}
			},
			want: `hello capability "future.capability" is not stable`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			events := test.events()
			test.mutate(events)
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}
}

func TestContract_StableValueDiagnosticsAreBounded(t *testing.T) {
	t.Parallel()

	longValue := strings.Repeat("值", 2048)
	for _, test := range []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		mutate   func([]map[string]any)
	}{
		{
			name:     "code",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["code"] = longValue
				lastObject(events)["code"] = longValue
			},
		},
		{
			name:     "stage",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				lastObject(events)["stage"] = longValue
			},
		},
		{
			name:     "remediation",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["remediation"] = []string{longValue}
				lastObject(events)["remediation"] = []string{longValue}
			},
		},
		{
			name:     "progress status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(progressObjectForContract(2)) },
			mutate: func(events []map[string]any) {
				events[1]["status"] = longValue
			},
		},
		{
			name:     "state status",
			terminal: TerminalSuccess,
			events:   func() []map[string]any { return successWithMiddle(stateObjectForContract(2)) },
			mutate: func(events []map[string]any) {
				events[1]["status"] = longValue
			},
		},
		{
			name:     "capability",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				events[0]["capabilities"] = []string{longValue}
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			events := test.events()
			test.mutate(events)
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, events))
			diagnostic := findIssue(t, issues, "is not stable").Error()
			if len(diagnostic) > 1024 {
				t.Fatalf("stable-value diagnostic length = %d, want at most 1024", len(diagnostic))
			}
			if !strings.Contains(diagnostic, diagnosticTruncationMarker) {
				t.Errorf("stable-value diagnostic lacks truncation marker: %q", diagnostic)
			}
			if !utf8.ValidString(diagnostic) {
				t.Errorf("stable-value diagnostic is invalid UTF-8: %q", diagnostic)
			}
		})
	}
}

func TestContract_ErrorDefinitions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		mutate   func([]map[string]any)
		want     string
	}{
		{
			name:     "error retryable differs from definition",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["retryable"] = true
				lastObject(events)["retryable"] = true
			},
			want: `error retryable must match definition for code "UPDATE_STATE_AMBIGUOUS"`,
		},
		{
			name:     "result remediation differs from definition",
			terminal: TerminalFailure,
			events:   stableFailureObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["remediation"] = []string{"retry"}
				lastObject(events)["remediation"] = []string{"retry"}
			},
			want: `result remediation must match definition for code "UPDATE_STATE_AMBIGUOUS"`,
		},
		{
			name:     "warning uses success code",
			terminal: TerminalSuccess,
			events:   stableWarningObjectsForContract,
			mutate: func(events []map[string]any) {
				events[1]["code"] = string(protocol.CodeOK)
				warningSummaryForContract(events)["code"] = string(protocol.CodeOK)
			},
			want: `warning code "OK" has no error definition`,
		},
		{
			name:     "success result has remediation",
			terminal: TerminalSuccess,
			events:   successObjects,
			mutate: func(events []map[string]any) {
				lastObject(events)["remediation"] = []string{"retry"}
			},
			want: "success result remediation must be empty",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			events := test.events()
			test.mutate(events)
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}
}

func TestContract_RejectsWarningOnlyFailureCodes(t *testing.T) {
	t.Parallel()

	for _, definition := range protocol.AllErrorDefinitions() {
		if definition.ExitCode != protocol.ExitCodeSuccess {
			continue
		}
		definition := definition
		t.Run(string(definition.Code), func(t *testing.T) {
			t.Parallel()

			remediation := make([]string, len(definition.Remediation))
			for index, value := range definition.Remediation {
				remediation[index] = string(value)
			}
			events := []map[string]any{
				helloObject(),
				errorObject(
					2,
					string(definition.Code),
					string(protocol.StageDoctor),
					definition.Retryable,
					remediation,
				),
				terminalResultObject(
					3,
					false,
					string(definition.Code),
					string(protocol.StageDoctor),
					"failed",
					definition.Retryable,
					remediation,
				),
			}
			_, issues := inspect(testCommand, TerminalFailure, encodeTranscript(t, events))
			requireIssue(t, issues, `error code "`+string(definition.Code)+`" is warning-only`)
			requireIssue(t, issues, `result code "`+string(definition.Code)+`" is warning-only`)
		})
	}
}

func successWithMiddle(event map[string]any) []map[string]any {
	return []map[string]any{helloObject(), event, successResultObject(3)}
}

func progressObjectForContract(sequence int) map[string]any {
	event := eventObject(string(protocol.TypeProgress), sequence)
	event["stage"] = string(protocol.StageDoctor)
	event["status"] = string(protocol.ProgressRunning)
	event["current"] = 1
	event["total"] = 2
	event["percent"] = 50
	event["message"] = "running"
	return event
}

func stateObjectForContract(sequence int) map[string]any {
	event := eventObject(string(protocol.TypeState), sequence)
	event["stage"] = string(protocol.StageDoctor)
	event["status"] = string(protocol.StateReadyToStart)
	event["message"] = "ready"
	event["details"] = map[string]any{}
	return event
}

func logObjectForContract(sequence int) map[string]any {
	event := eventObject(string(protocol.TypeLog), sequence)
	event["source"] = "runtime"
	event["stream"] = "stderr"
	event["message"] = "diagnostic"
	return event
}

func stableFailureObjectsForContract() []map[string]any {
	const code = protocol.CodeUpdateStateAmbiguous
	remediation := []string{
		string(protocol.RemediationRunDoctor),
		string(protocol.RemediationContactSupport),
	}
	return []map[string]any{
		helloObject(),
		errorObject(2, string(code), string(protocol.StageDoctor), false, remediation),
		terminalResultObject(
			3,
			false,
			string(code),
			string(protocol.StageDoctor),
			"failed",
			false,
			remediation,
		),
	}
}

func stableWarningObjectsForContract() []map[string]any {
	return warningObjects(1)
}

func warningSummaryForContract(events []map[string]any) map[string]any {
	details := lastObject(events)["details"].(map[string]any)
	return details["warnings"].([]any)[0].(map[string]any)
}
