package contracttest

import (
	"encoding/json"
	"fmt"
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
	diagnostic := issues[0].Error()
	for _, want := range []string{
		"command=\"dependencies sync\"",
		"scenario=\"failure\"",
		"line=",
		"raw=",
		"types=",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic %q does not contain %q", diagnostic, want)
		}
	}
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

func helloObject() map[string]any {
	return eventObject("hello", 1)
}

func resultObject(sequence int) map[string]any {
	event := eventObject("result", sequence)
	event["success"] = true
	event["code"] = "OK"
	event["stage"] = "doctor"
	event["status"] = "succeeded"
	event["message"] = "done"
	event["retryable"] = false
	event["remediation"] = []string{}
	event["details"] = map[string]any{}
	return event
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
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(event) error = %v", err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func requireIssue(t *testing.T, issues []contractIssue, want string) {
	t.Helper()
	for _, issue := range issues {
		if strings.Contains(issue.Error(), want) {
			return
		}
	}
	t.Fatalf("issues = %v, want one containing %q", issues, want)
}

func (i contractIssue) GoString() string {
	return fmt.Sprintf("%q", i.Error())
}
