package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFormatDiagnostic_UsesExactUppercaseLevelAndCompactDetails(t *testing.T) {
	tests := []struct {
		name  string
		level Level
		want  string
	}{
		{
			name:  "debug",
			level: LevelDebug,
			want:  "2026-07-29T18:20:30.1234567+08:00 DEBUG [workspace-sync] [01JTEST] 仓库校验完成 {\"commit\":\"0123\"}\n",
		},
		{
			name:  "info",
			level: LevelInfo,
			want:  "2026-07-29T18:20:30.1234567+08:00 INFO [workspace-sync] [01JTEST] 仓库校验完成 {\"commit\":\"0123\"}\n",
		},
		{
			name:  "warn",
			level: LevelWarn,
			want:  "2026-07-29T18:20:30.1234567+08:00 WARN [workspace-sync] [01JTEST] 仓库校验完成 {\"commit\":\"0123\"}\n",
		},
		{
			name:  "error",
			level: LevelError,
			want:  "2026-07-29T18:20:30.1234567+08:00 ERROR [workspace-sync] [01JTEST] 仓库校验完成 {\"commit\":\"0123\"}\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := string(formatDiagnostic(
				fixedEntryTime(),
				test.level,
				"workspace-sync",
				"01JTEST",
				"仓库校验完成",
				json.RawMessage(`{"commit":"0123"}`),
			))
			if got != test.want {
				t.Fatalf("formatDiagnostic() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatDiagnostic_EscapesCommandOperationIDAndMessageWithOneRule(t *testing.T) {
	values := []string{
		"command\r\n\t\x00\u007f\u0085\u200b",
		"operation\r\n\t\x01\u007f\u0085\u200e",
		"message\r\n\t\x02\u007f\u0085\u2060",
	}
	got := string(formatDiagnostic(
		fixedEntryTime(),
		LevelWarn,
		values[0],
		values[1],
		values[2],
		json.RawMessage(`{"text":"甲乙"}`),
	))
	for _, value := range values {
		escaped := escapeVisible(value)
		if !strings.Contains(got, escaped) {
			t.Fatalf("diagnostic %q does not contain escaped value %q", got, escaped)
		}
	}
	for _, want := range []string{
		`\r`, `\n`, `\t`, `\u0000`, `\u0001`, `\u0002`,
		`\u007F`, `\u0085`, `\u200B`, `\u200E`, `\u2060`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic %q does not contain visible escape %q", got, want)
		}
	}
	if strings.Contains(got, "\r") || bytes.Count([]byte(got), []byte{'\n'}) != 1 {
		t.Fatalf("diagnostic has injected physical line: %q", got)
	}
}

func TestFormatDiagnostic_ProducesOnePhysicalLine(t *testing.T) {
	got := formatDiagnostic(
		fixedEntryTime(),
		LevelError,
		"doctor",
		"01JTEST",
		"first\nsecond\rthird",
		json.RawMessage(`{"line":"a\nb"}`),
	)
	if bytes.Count(got, []byte{'\n'}) != 1 || got[len(got)-1] != '\n' {
		t.Fatalf("physical LF count = %d, want one trailing LF", bytes.Count(got, []byte{'\n'}))
	}
}

func TestWriteDiagnosticSinks_AttemptsBothSinks(t *testing.T) {
	fileCause := errors.New("file")
	stderrCalls := 0
	result, err := writeDiagnosticSinks(
		entryWriterFunc(func([]byte) (int, error) { return 0, fileCause }),
		entryWriterFunc(func(p []byte) (int, error) {
			stderrCalls++
			return len(p), nil
		}),
		[]byte("file\n"),
		[]byte("stderr\n"),
	)
	if result.FileWritten || !result.StderrWritten || stderrCalls != 1 {
		t.Fatalf("result = %#v, stderr calls = %d, want only stderr written once", result, stderrCalls)
	}
	if !errors.Is(err, fileCause) {
		t.Fatalf("error = %v, want file cause", err)
	}
}

func TestWriteDiagnosticSinks_ReportsIndependentResults(t *testing.T) {
	cause := errors.New("sink")
	tests := []struct {
		name       string
		fileN      int
		fileErr    error
		stderrN    int
		stderrErr  error
		wantFile   bool
		wantStderr bool
	}{
		{name: "both full", fileN: 5, stderrN: 7, wantFile: true, wantStderr: true},
		{name: "file partial stderr zero", fileN: 1, stderrErr: cause, wantFile: true},
		{name: "file zero stderr full error", fileErr: cause, stderrN: 7, stderrErr: cause, wantStderr: true},
		{name: "both zero", fileErr: cause, stderrErr: cause},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, _ := writeDiagnosticSinks(
				entryWriterFunc(func([]byte) (int, error) { return test.fileN, test.fileErr }),
				entryWriterFunc(func([]byte) (int, error) { return test.stderrN, test.stderrErr }),
				[]byte("file\n"),
				[]byte("stderr\n"),
			)
			if result.FileWritten != test.wantFile || result.StderrWritten != test.wantStderr {
				t.Fatalf("result = %#v, want file=%v stderr=%v", result, test.wantFile, test.wantStderr)
			}
		})
	}
}

func TestWriteDiagnosticSinks_JoinsFileAndStderrErrors(t *testing.T) {
	fileCause := errors.New("file")
	stderrCause := errors.New("stderr")
	result, err := writeDiagnosticSinks(
		entryWriterFunc(func(p []byte) (int, error) { return len(p), fileCause }),
		entryWriterFunc(func(p []byte) (int, error) { return len(p), stderrCause }),
		[]byte("file\n"),
		[]byte("stderr\n"),
	)
	if !result.FileWritten || !result.StderrWritten {
		t.Fatalf("result = %#v, want both written", result)
	}
	if !errors.Is(err, fileCause) || !errors.Is(err, stderrCause) {
		t.Fatalf("error = %v, want both sink causes", err)
	}
}
