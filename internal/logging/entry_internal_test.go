package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type entryWriterFunc func([]byte) (int, error)

func (f entryWriterFunc) Write(p []byte) (int, error) {
	return f(p)
}

type changingMarshaler struct {
	calls int
}

func (m *changingMarshaler) MarshalJSON() ([]byte, error) {
	m.calls++
	return []byte(fmt.Sprintf(`{"call":%d}`, m.calls)), nil
}

func fixedEntryTime() time.Time {
	return time.Date(2026, 7, 29, 18, 20, 30, 123456700, time.FixedZone("CST", 8*60*60))
}

func decodeEntryLine(t *testing.T, line []byte) map[string]json.RawMessage {
	t.Helper()
	if got := bytes.Count(line, []byte{'\n'}); got != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("physical LF count = %d, want 1 trailing LF", got)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return decoded
}

func TestEncodeEntry_UsesExactSchemaAndRoundTripsAllLevels(t *testing.T) {
	tests := []struct {
		name        string
		level       Level
		kind        entryKind
		command     string
		operationID string
		message     string
		detail      string
	}{
		{
			name:        "debug diagnostic",
			level:       LevelDebug,
			kind:        entryDiagnostic,
			command:     "doctor",
			operationID: "01JDEBUG",
			message:     "debug message",
			detail:      "debug detail",
		},
		{
			name:        "info operation",
			level:       LevelInfo,
			kind:        entryOperation,
			command:     "workspace-sync",
			operationID: "01JINFO",
			message:     "仓库校验完成",
			detail:      "提交 \"0123\"",
		},
		{
			name:        "warn diagnostic controls quotes and non ASCII",
			level:       LevelWarn,
			kind:        entryDiagnostic,
			command:     "同步\r\n\t\"命令",
			operationID: "操作\r\n\t\"甲",
			message:     "第一行\r\n\t\"第二行",
			detail:      "明细\r\n\t\"乙",
		},
		{
			name:        "error operation",
			level:       LevelError,
			kind:        entryOperation,
			command:     "repair",
			operationID: "01JERROR",
			message:     "修复失败 \"磁盘\"",
			detail:      "错误详情",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodeEntry(
				fixedEntryTime(),
				test.level,
				test.kind,
				test.command,
				test.operationID,
				test.message,
				map[string]any{"text": test.detail},
			)
			if err != nil {
				t.Fatalf("encodeEntry() error = %v", err)
			}
			decoded := decodeEntryLine(t, encoded.line)
			wantKeys := []string{
				"timestamp", "level", "kind", "command", "operationId", "message", "details",
			}
			if len(decoded) != len(wantKeys) {
				t.Fatalf("schema key count = %d, want %d", len(decoded), len(wantKeys))
			}
			for _, key := range wantKeys {
				if _, ok := decoded[key]; !ok {
					t.Fatalf("schema is missing key %q", key)
				}
			}

			var roundTrip struct {
				Timestamp   string            `json:"timestamp"`
				Level       string            `json:"level"`
				Kind        string            `json:"kind"`
				Command     string            `json:"command"`
				OperationID string            `json:"operationId"`
				Message     string            `json:"message"`
				Details     map[string]string `json:"details"`
			}
			if err := json.Unmarshal(
				bytes.TrimSuffix(encoded.line, []byte{'\n'}),
				&roundTrip,
			); err != nil {
				t.Fatalf("decode entry: %v", err)
			}
			if want := fixedEntryTime().Format(time.RFC3339Nano); roundTrip.Timestamp != want {
				t.Fatalf("timestamp = %q, want %q", roundTrip.Timestamp, want)
			}
			if roundTrip.Level != test.level.String() {
				t.Fatalf("level = %q, want %q", roundTrip.Level, test.level)
			}
			if roundTrip.Kind != string(test.kind) {
				t.Fatalf("kind = %q, want %q", roundTrip.Kind, test.kind)
			}
			if roundTrip.Command != test.command {
				t.Fatalf("command = %q, want %q", roundTrip.Command, test.command)
			}
			if roundTrip.OperationID != test.operationID {
				t.Fatalf("operationId = %q, want %q", roundTrip.OperationID, test.operationID)
			}
			if roundTrip.Message != test.message {
				t.Fatalf("message = %q, want %q", roundTrip.Message, test.message)
			}
			if got := roundTrip.Details["text"]; got != test.detail {
				t.Fatalf("details.text = %q, want %q", got, test.detail)
			}
			if !bytes.Equal(decoded["details"], encoded.detailsJSON) {
				t.Fatalf(
					"wire details = %s, want exact RawMessage %s",
					decoded["details"],
					encoded.detailsJSON,
				)
			}
		})
	}
}

func TestEncodeEntry_NormalizesNilDetailsToObject(t *testing.T) {
	encoded, err := encodeEntry(
		fixedEntryTime(), LevelDebug, entryDiagnostic, "doctor", "01JTEST", "", nil,
	)
	if err != nil {
		t.Fatalf("encodeEntry() error = %v", err)
	}
	if got := string(encoded.detailsJSON); got != "{}" {
		t.Fatalf("detailsJSON = %q, want {}", got)
	}
	if got := string(decodeEntryLine(t, encoded.line)["details"]); got != "{}" {
		t.Fatalf("wire details = %q, want {}", got)
	}
}

func TestEncodeEntry_EscapesControlsAndProducesOneUTF8Line(t *testing.T) {
	message := "第一行\r\n\t\"第二行" + string([]byte{0xff})
	encoded, err := encodeEntry(
		fixedEntryTime(),
		LevelWarn,
		entryDiagnostic,
		"workspace-sync",
		"01JTEST",
		message,
		map[string]any{"text": "甲\n乙\t\""},
	)
	if err != nil {
		t.Fatalf("encodeEntry() error = %v", err)
	}
	if !utf8.Valid(encoded.line) {
		t.Fatal("encoded line is not valid UTF-8")
	}
	if got := bytes.Count(encoded.line, []byte{'\n'}); got != 1 {
		t.Fatalf("physical LF count = %d, want 1", got)
	}
	decodeEntryLine(t, encoded.line)
	if strings.Contains(string(bytes.TrimSuffix(encoded.line, []byte{'\n'})), "\r") {
		t.Fatal("encoded JSON contains a physical CR")
	}
}

func TestEncodeEntry_RejectsInvalidLevelTimeAndDetails(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	tests := []struct {
		name    string
		now     time.Time
		level   Level
		details map[string]any
		want    error
	}{
		{name: "level", now: fixedEntryTime(), level: Level("trace"), details: nil, want: ErrInvalidLevel},
		{name: "time", now: time.Time{}, level: LevelInfo, details: nil, want: ErrInvalidTime},
		{name: "channel", now: fixedEntryTime(), level: LevelInfo, details: map[string]any{"bad": make(chan int)}, want: ErrEncodeEntry},
		{name: "function", now: fixedEntryTime(), level: LevelInfo, details: map[string]any{"bad": func() {}}, want: ErrEncodeEntry},
		{name: "cycle", now: fixedEntryTime(), level: LevelInfo, details: cycle, want: ErrEncodeEntry},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := encodeEntry(
				test.now, test.level, entryOperation, "doctor", "01JTEST", "message", test.details,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("encodeEntry() error = %v, want errors.Is(_, %v)", err, test.want)
			}
			if got.line != nil || got.detailsJSON != nil {
				t.Fatalf("encodeEntry() = %#v, want zero encodedEntry", got)
			}
		})
	}
}

func TestEncodeEntry_EncodesDetailsExactlyOnceAsRawMessage(t *testing.T) {
	value := &changingMarshaler{}
	encoded, err := encodeEntry(
		fixedEntryTime(),
		LevelInfo,
		entryDiagnostic,
		"doctor",
		"01JTEST",
		"message",
		map[string]any{"value": value},
	)
	if err != nil {
		t.Fatalf("encodeEntry() error = %v", err)
	}
	if value.calls != 1 {
		t.Fatalf("MarshalJSON calls = %d, want 1", value.calls)
	}
	if got, want := string(encoded.detailsJSON), `{"value":{"call":1}}`; got != want {
		t.Fatalf("detailsJSON = %s, want %s", got, want)
	}
	if got := decodeEntryLine(t, encoded.line)["details"]; !bytes.Equal(got, encoded.detailsJSON) {
		t.Fatalf("wire details = %s, want exact %s", got, encoded.detailsJSON)
	}
}

func TestWriteLine_PerformsOneSynchronousWrite(t *testing.T) {
	line := []byte("{\"value\":1}\n")
	calls := 0
	var got []byte
	written, err := writeLine(entryWriterFunc(func(p []byte) (int, error) {
		calls++
		got = append([]byte(nil), p...)
		return len(p), nil
	}), line)
	if err != nil {
		t.Fatalf("writeLine() error = %v", err)
	}
	if !written || calls != 1 || !bytes.Equal(got, line) {
		t.Fatalf("writeLine() = (%v, calls %d, %q), want (true, 1, %q)", written, calls, got, line)
	}
}

func TestWriteLine_ReportsPartialWriteAsWritten(t *testing.T) {
	cause := errors.New("partial")
	for _, resultErr := range []error{nil, cause} {
		calls := 0
		written, err := writeLine(entryWriterFunc(func(p []byte) (int, error) {
			calls++
			return len(p) - 1, resultErr
		}), []byte("line\n"))
		if !written || calls != 1 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("writeLine(partial, %v) = (%v, %v, calls %d), want written short write once", resultErr, written, err, calls)
		}
		if resultErr != nil && !errors.Is(err, resultErr) {
			t.Fatalf("writeLine(partial) error = %v, want cause", err)
		}
	}
}

func TestWriteLine_ReportsFullWriteWithErrorAsWritten(t *testing.T) {
	cause := errors.New("full write error")
	written, err := writeLine(entryWriterFunc(func(p []byte) (int, error) {
		return len(p), cause
	}), []byte("line\n"))
	if !written || !errors.Is(err, cause) {
		t.Fatalf("writeLine(full with error) = (%v, %v), want (true, cause)", written, err)
	}
}

func TestWriteLine_ReportsZeroWriteAsNotWritten(t *testing.T) {
	cause := errors.New("zero write error")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil error", err: nil, want: io.ErrShortWrite},
		{name: "cause", err: cause, want: cause},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			written, err := writeLine(entryWriterFunc(func([]byte) (int, error) {
				return 0, test.err
			}), []byte("line\n"))
			if written || !errors.Is(err, test.want) {
				t.Fatalf("writeLine(zero) = (%v, %v), want (false, %v)", written, err, test.want)
			}
		})
	}
}
