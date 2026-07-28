package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const (
	testCommand     = "doctor"
	testOperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testTimestamp   = "2026-07-28T08:00:00Z"
)

func TestContract_ParsePhysicalLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout []byte
		want   string
	}{
		{name: "empty output", stdout: nil, want: "transcript is empty"},
		{name: "missing LF", stdout: []byte(`{"type":"hello"}`), want: "must end with LF"},
		{name: "empty physical line", stdout: []byte("{}\n\n"), want: "empty physical line"},
		{name: "CRLF", stdout: []byte("{}\r\n"), want: "contains CR"},
		{name: "scalar", stdout: []byte("42\n"), want: "must be a JSON object"},
		{name: "array", stdout: []byte("[]\n"), want: "must be a JSON object"},
		{name: "two JSON values", stdout: []byte("{}{}\n"), want: "trailing JSON value"},
		{name: "trailing token", stdout: []byte("{} nope\n"), want: "trailing token"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, issues := inspect(testCommand, TerminalSuccess, test.stdout)
			requireIssue(t, issues, test.want)
		})
	}

	events, issues := inspect(testCommand, TerminalSuccess, validTranscript())
	if len(issues) != 0 {
		t.Fatalf("valid transcript issues = %v", issues)
	}
	if len(events) != 2 {
		t.Fatalf("parsed event count = %d, want 2", len(events))
	}
	if sequence, ok := events[1].object["sequence"].(json.Number); !ok || sequence.String() != "2" {
		t.Fatalf("result sequence = %#v (%T), want json.Number(2)", events[1].object["sequence"], events[1].object["sequence"])
	}

	withoutFinalLF := bytes.TrimSuffix(validTranscript(), []byte{'\n'})
	lastLF := bytes.LastIndexByte(withoutFinalLF, '\n')
	wantRaw := withoutFinalLF[lastLF+1:]
	_, issues = inspect(testCommand, TerminalSuccess, withoutFinalLF)
	issue := findIssue(t, issues, "must end with LF")
	if issue.line != 2 {
		t.Errorf("missing-LF issue line = %d, want 2", issue.line)
	}
	if !bytes.Equal(issue.raw, wantRaw) {
		t.Errorf("missing-LF issue raw = %q, want final physical line %q", issue.raw, wantRaw)
	}
}

func TestContract_Envelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func([]map[string]any)
		want   string
	}{
		{
			name: "first event is not hello",
			mutate: func(events []map[string]any) {
				events[0]["type"] = "progress"
			},
			want: "first event must be hello",
		},
		{
			name: "second hello",
			mutate: func(events []map[string]any) {
				events[1]["type"] = "hello"
			},
			want: "hello must appear exactly once",
		},
		{
			name: "protocol mismatch",
			mutate: func(events []map[string]any) {
				events[1]["protocol"] = 2
			},
			want: "protocol must equal 1",
		},
		{
			name: "operation ID mismatch",
			mutate: func(events []map[string]any) {
				events[1]["operationId"] = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			},
			want: "operationId must match",
		},
		{
			name: "operation ID is not canonical",
			mutate: func(events []map[string]any) {
				events[0]["operationId"] = strings.ToLower(testOperationID)
				events[1]["operationId"] = strings.ToLower(testOperationID)
			},
			want: "operationId must be a canonical ULID",
		},
		{
			name: "sequence gap",
			mutate: func(events []map[string]any) {
				events[1]["sequence"] = 3
			},
			want: "sequence must equal physical line 2",
		},
		{
			name: "sequence duplicate",
			mutate: func(events []map[string]any) {
				events[1]["sequence"] = 1
			},
			want: "sequence must equal physical line 2",
		},
		{
			name: "sequence is not integer",
			mutate: func(events []map[string]any) {
				events[1]["sequence"] = 2.5
			},
			want: "sequence must be an integer",
		},
		{
			name: "invalid timestamp",
			mutate: func(events []map[string]any) {
				events[1]["timestamp"] = "yesterday"
			},
			want: "timestamp must be RFC3339Nano",
		},
		{
			name: "missing type",
			mutate: func(events []map[string]any) {
				delete(events[1], "type")
			},
			want: "type must be a string",
		},
		{
			name: "non string type",
			mutate: func(events []map[string]any) {
				events[1]["type"] = 7
			},
			want: "type must be a string",
		},
		{
			name: "unknown type",
			mutate: func(events []map[string]any) {
				events[1]["type"] = "mystery"
			},
			want: "unknown event type",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			events := validObjects()
			test.mutate(events)
			_, issues := inspect(testCommand, TerminalSuccess, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}
}

func TestContract_ResultPlacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []map[string]any
		want   string
	}{
		{
			name:   "missing result",
			events: []map[string]any{helloObject()},
			want:   "result must appear exactly once",
		},
		{
			name: "duplicate result",
			events: []map[string]any{
				helloObject(),
				resultObject(2),
				resultObject(3),
			},
			want: "result must appear exactly once",
		},
		{
			name: "result is not last",
			events: []map[string]any{
				helloObject(),
				resultObject(2),
				eventObject("log", 3),
			},
			want: "result must be the last event",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, issues := inspect(testCommand, TerminalSuccess, encodeTranscript(t, test.events))
			requireIssue(t, issues, test.want)
		})
	}

	t.Run("invalid line before result does not hide a later event", func(t *testing.T) {
		stdout := joinPhysicalLines(
			encodeObject(t, helloObject()),
			[]byte("not-json"),
			encodeObject(t, resultObject(3)),
			encodeObject(t, eventObject("log", 4)),
		)
		_, issues := inspect(testCommand, TerminalSuccess, stdout)
		requireIssue(t, issues, "result must be the last event")
	})

	t.Run("invalid line before a final result does not create false placement issue", func(t *testing.T) {
		stdout := joinPhysicalLines(
			encodeObject(t, helloObject()),
			[]byte("not-json"),
			encodeObject(t, resultObject(3)),
		)
		_, issues := inspect(testCommand, TerminalSuccess, stdout)
		requireIssue(t, issues, "invalid JSON")
		requireNoIssue(t, issues, "result must be the last event")
	})

	t.Run("invalid line after result is reported at its physical position", func(t *testing.T) {
		stdout := joinPhysicalLines(
			encodeObject(t, helloObject()),
			encodeObject(t, resultObject(2)),
			[]byte("not-json"),
		)
		_, issues := inspect(testCommand, TerminalSuccess, stdout)
		issue := findIssue(t, issues, "invalid JSON")
		if issue.line != 3 || string(issue.raw) != "not-json" {
			t.Errorf("invalid trailing line issue = line %d raw %q, want line 3 raw %q", issue.line, issue.raw, "not-json")
		}
		requireNoIssue(t, issues, "result must be the last event")
	})
}

func TestContract_Diagnostics(t *testing.T) {
	t.Parallel()

	_, issues := inspect("dependencies sync", TerminalFailure, []byte(
		`{"protocol":1,"type":"hello","operationId":"bad","sequence":1,"timestamp":"2026-07-28T08:00:00Z"}`+"\n"+
			`{"protocol":1,"type":"mystery","operationId":"bad","sequence":2,"timestamp":"2026-07-28T08:00:00Z"}`+"\n",
	))
	if len(issues) == 0 {
		t.Fatal("inspect() issues = nil, want diagnostics")
	}
	diagnostic := findIssue(t, issues, `unknown event type "mystery"`).Error()
	for _, want := range []string{
		"command=\"dependencies sync\"",
		"scenario=\"failure\"",
		"line=2",
		"raw=",
		"types=[hello,mystery]",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic %q does not contain %q", diagnostic, want)
		}
	}

	raw := append([]byte("prefix\t\"quoted\"\\path-"), bytes.Repeat([]byte{'x'}, 200)...)
	truncated := newIssue("doctor", TerminalCancelled, 7, raw, "bad output")
	truncated.types = "[hello,result]"
	truncatedDiagnostic := truncated.Error()
	wantQuotedRaw := strconv.Quote(string(raw[:160])) + "...(truncated)"
	for _, want := range []string{
		"command=\"doctor\"",
		"scenario=\"cancelled\"",
		"line=7",
		"raw=" + wantQuotedRaw,
		"types=[hello,result]",
	} {
		if !strings.Contains(truncatedDiagnostic, want) {
			t.Errorf("truncated diagnostic %q does not contain %q", truncatedDiagnostic, want)
		}
	}
}

func TestContract_TerminalSemantics(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		terminal Terminal
		events   []map[string]any
	}{
		{name: "success", terminal: TerminalSuccess, events: successObjects()},
		{name: "success with warning", terminal: TerminalSuccess, events: warningObjects(1)},
		{name: "failure", terminal: TerminalFailure, events: failureObjects()},
		{name: "cancelled", terminal: TerminalCancelled, events: cancelledObjects()},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, issues := inspect(testCommand, test.terminal, encodeTranscript(t, test.events))
			requireNoIssues(t, issues)
		})
	}

	tests := []struct {
		name     string
		terminal Terminal
		events   func() []map[string]any
		mutate   func([]map[string]any)
		want     string
	}{
		{
			name: "success flag false", terminal: TerminalSuccess, events: successObjects,
			mutate: func(events []map[string]any) { lastObject(events)["success"] = false },
			want:   "success result must have success=true",
		},
		{
			name: "success code", terminal: TerminalSuccess, events: successObjects,
			mutate: func(events []map[string]any) { lastObject(events)["code"] = "FAILED" },
			want:   "success result code must be OK",
		},
		{
			name: "success retryable", terminal: TerminalSuccess, events: successObjects,
			mutate: func(events []map[string]any) { lastObject(events)["retryable"] = true },
			want:   "success result must not be retryable",
		},
		{
			name: "success cancelled status", terminal: TerminalSuccess, events: successObjects,
			mutate: func(events []map[string]any) { lastObject(events)["status"] = "cancelled" },
			want:   "success result status must not be cancelled",
		},
		{
			name: "success preceded by error", terminal: TerminalSuccess,
			events: func() []map[string]any {
				return []map[string]any{
					helloObject(),
					errorObject(2, "FAILED", "doctor", false, []string{"retry"}),
					successResultObject(3),
				}
			},
			mutate: func([]map[string]any) {},
			want:   "success transcript must not contain error",
		},
		{
			name: "failure flag true", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { lastObject(events)["success"] = true },
			want:   "failure result must have success=false",
		},
		{
			name: "failure OK code", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { lastObject(events)["code"] = "OK" },
			want:   "failure result code must not be OK or OPERATION_CANCELLED",
		},
		{
			name: "failure cancellation code", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { lastObject(events)["code"] = "OPERATION_CANCELLED" },
			want:   "failure result code must not be OK or OPERATION_CANCELLED",
		},
		{
			name: "failure cancelled status", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { lastObject(events)["status"] = "cancelled" },
			want:   "failure result status must not be cancelled",
		},
		{
			name: "failure missing matching error", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { events[1]["code"] = "OTHER_FAILURE" },
			want:   "failure result must match a prior error",
		},
		{
			name: "failure partial tuple match", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) { events[1]["remediation"] = []string{"different"} },
			want:   "failure result must match a prior error",
		},
		{
			name: "failure tuple field missing from both events", terminal: TerminalFailure, events: failureObjects,
			mutate: func(events []map[string]any) {
				delete(events[1], "remediation")
				delete(lastObject(events), "remediation")
			},
			want: "failure result must match a prior error",
		},
		{
			name: "cancelled flag true", terminal: TerminalCancelled, events: cancelledObjects,
			mutate: func(events []map[string]any) { lastObject(events)["success"] = true },
			want:   "cancelled result must have success=false",
		},
		{
			name: "cancelled wrong code", terminal: TerminalCancelled, events: cancelledObjects,
			mutate: func(events []map[string]any) { lastObject(events)["code"] = "FAILED" },
			want:   "cancelled result code must be OPERATION_CANCELLED",
		},
		{
			name: "cancelled wrong status", terminal: TerminalCancelled, events: cancelledObjects,
			mutate: func(events []map[string]any) { lastObject(events)["status"] = "failed" },
			want:   "cancelled result status must be cancelled",
		},
		{
			name: "cancelled missing matching error", terminal: TerminalCancelled, events: cancelledObjects,
			mutate: func(events []map[string]any) { events[1]["retryable"] = true },
			want:   "cancelled result must match a prior error",
		},
	}

	for _, test := range tests {
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

func TestContract_WarningSummary(t *testing.T) {
	t.Parallel()

	for _, count := range []int{0, 1, 256, 257} {
		count := count
		t.Run(fmt.Sprintf("%d warnings", count), func(t *testing.T) {
			t.Parallel()
			_, issues := inspect(testCommand, TerminalSuccess, encodeTranscript(t, warningObjects(count)))
			requireNoIssues(t, issues)
		})
	}

	tests := []struct {
		name   string
		count  int
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "missing warnings", count: 1,
			mutate: func(details map[string]any) { delete(details, "warnings") },
			want:   "result warnings must be present",
		},
		{
			name: "missing warning count", count: 1,
			mutate: func(details map[string]any) { delete(details, "warningCount") },
			want:   "result warningCount must be present",
		},
		{
			name: "missing truncated", count: 1,
			mutate: func(details map[string]any) { delete(details, "warningsTruncated") },
			want:   "result warningsTruncated must be present",
		},
		{
			name: "missing summary", count: 2,
			mutate: func(details map[string]any) {
				summaries := details["warnings"].([]any)
				details["warnings"] = summaries[:1]
			},
			want: "result warnings must equal the earliest warning events",
		},
		{
			name: "extra summary", count: 1,
			mutate: func(details map[string]any) {
				summaries := details["warnings"].([]any)
				details["warnings"] = append(summaries, warningSummaryObject(99))
			},
			want: "result warnings must equal the earliest warning events",
		},
		{
			name: "reordered summaries", count: 2,
			mutate: func(details map[string]any) {
				summaries := details["warnings"].([]any)
				summaries[0], summaries[1] = summaries[1], summaries[0]
			},
			want: "result warnings must equal the earliest warning events",
		},
		{
			name: "summary field differs", count: 1,
			mutate: func(details map[string]any) {
				summary := details["warnings"].([]any)[0].(map[string]any)
				summary["message"] = "forged"
			},
			want: "result warnings must equal the earliest warning events",
		},
		{
			name: "wrong count", count: 1,
			mutate: func(details map[string]any) { details["warningCount"] = 2 },
			want:   "result warningCount must equal 1",
		},
		{
			name: "wrong truncated at limit", count: 256,
			mutate: func(details map[string]any) { details["warningsTruncated"] = true },
			want:   "result warningsTruncated must equal false",
		},
		{
			name: "wrong truncated above limit", count: 257,
			mutate: func(details map[string]any) { details["warningsTruncated"] = false },
			want:   "result warningsTruncated must equal true",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			events := warningObjects(test.count)
			test.mutate(lastObject(events)["details"].(map[string]any))
			_, issues := inspect(testCommand, TerminalSuccess, encodeTranscript(t, events))
			requireIssue(t, issues, test.want)
		})
	}

	t.Run("forged summary without warnings", func(t *testing.T) {
		t.Parallel()
		events := warningObjects(0)
		lastObject(events)["details"] = map[string]any{
			"warnings":          []any{warningSummaryObject(0)},
			"warningCount":      1,
			"warningsTruncated": false,
		}
		_, issues := inspect(testCommand, TerminalSuccess, encodeTranscript(t, events))
		requireIssue(t, issues, "result warning summary keys must be absent when there are no warnings")
	})
}

func validTranscript() []byte {
	events := validObjects()
	var builder strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func validObjects() []map[string]any {
	return []map[string]any{helloObject(), resultObject(2)}
}

func successObjects() []map[string]any {
	return []map[string]any{helloObject(), successResultObject(2)}
}

func failureObjects() []map[string]any {
	return []map[string]any{
		helloObject(),
		errorObject(2, "DOCTOR_FAILED", "doctor", true, []string{"retry", "open-log"}),
		terminalResultObject(3, false, "DOCTOR_FAILED", "doctor", "failed", true, []string{"retry", "open-log"}),
	}
}

func cancelledObjects() []map[string]any {
	return []map[string]any{
		helloObject(),
		errorObject(2, "OPERATION_CANCELLED", "doctor", false, []string{"retry"}),
		terminalResultObject(3, false, "OPERATION_CANCELLED", "doctor", "cancelled", false, []string{"retry"}),
	}
}

func warningObjects(count int) []map[string]any {
	events := make([]map[string]any, 0, count+2)
	events = append(events, helloObject())
	summaries := make([]any, 0, min(count, 256))
	for index := 0; index < count; index++ {
		events = append(events, warningObject(index+2, index))
		if index < 256 {
			summaries = append(summaries, warningSummaryObject(index))
		}
	}
	result := successResultObject(count + 2)
	if count > 0 {
		result["details"] = map[string]any{
			"warnings":          summaries,
			"warningCount":      count,
			"warningsTruncated": count > 256,
		}
	}
	events = append(events, result)
	return events
}

func helloObject() map[string]any {
	return eventObject("hello", 1)
}

func resultObject(sequence int) map[string]any {
	return successResultObject(sequence)
}

func successResultObject(sequence int) map[string]any {
	return terminalResultObject(sequence, true, "OK", "doctor", "succeeded", false, []string{})
}

func terminalResultObject(
	sequence int,
	success bool,
	code string,
	stage string,
	status string,
	retryable bool,
	remediation []string,
) map[string]any {
	event := eventObject("result", sequence)
	event["success"] = success
	event["code"] = code
	event["stage"] = stage
	event["status"] = status
	event["message"] = "done"
	event["retryable"] = retryable
	event["remediation"] = remediation
	event["details"] = map[string]any{}
	return event
}

func errorObject(sequence int, code string, stage string, retryable bool, remediation []string) map[string]any {
	event := eventObject("error", sequence)
	event["code"] = code
	event["stage"] = stage
	event["message"] = "failed"
	event["retryable"] = retryable
	event["remediation"] = remediation
	event["details"] = map[string]any{}
	return event
}

func warningObject(sequence int, index int) map[string]any {
	event := eventObject("warning", sequence)
	for key, value := range warningSummaryObject(index) {
		event[key] = value
	}
	return event
}

func warningSummaryObject(index int) map[string]any {
	return map[string]any{
		"code":        fmt.Sprintf("WARN_%03d", index),
		"stage":       "doctor",
		"message":     fmt.Sprintf("warning %d", index),
		"retryable":   index%2 == 0,
		"remediation": []string{fmt.Sprintf("action-%d", index)},
		"details": map[string]any{
			"index":  index,
			"nested": []any{fmt.Sprintf("value-%d", index), index%3 == 0},
		},
	}
}

func lastObject(events []map[string]any) map[string]any {
	return events[len(events)-1]
}

func eventObject(eventType string, sequence int) map[string]any {
	return map[string]any{
		"protocol":    1,
		"type":        eventType,
		"operationId": testOperationID,
		"sequence":    sequence,
		"timestamp":   testTimestamp,
	}
}

func encodeTranscript(t *testing.T, events []map[string]any) []byte {
	t.Helper()
	var builder strings.Builder
	for _, event := range events {
		builder.Write(encodeObject(t, event))
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func encodeObject(t *testing.T, event map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal(event) error = %v", err)
	}
	return encoded
}

func joinPhysicalLines(lines ...[]byte) []byte {
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

func requireIssue(t *testing.T, issues []contractIssue, want string) {
	t.Helper()
	_ = findIssue(t, issues, want)
}

func findIssue(t *testing.T, issues []contractIssue, want string) contractIssue {
	t.Helper()
	for _, issue := range issues {
		if strings.Contains(issue.Error(), want) {
			return issue
		}
	}
	t.Fatalf("issues = %v, want one containing %q", issues, want)
	return contractIssue{}
}

func requireNoIssue(t *testing.T, issues []contractIssue, forbidden string) {
	t.Helper()
	for _, issue := range issues {
		if strings.Contains(issue.Error(), forbidden) {
			t.Fatalf("issues = %v, want none containing %q", issues, forbidden)
		}
	}
}

func requireNoIssues(t *testing.T, issues []contractIssue) {
	t.Helper()
	if len(issues) != 0 {
		t.Fatalf("inspect() issues = %#v, want none", issues)
	}
}

func (i contractIssue) GoString() string {
	return fmt.Sprintf("%q", i.Error())
}
