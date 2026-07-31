package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type entryKind string

const (
	entryDiagnostic entryKind = "diagnostic"
	entryOperation  entryKind = "operation"
)

type entry struct {
	Timestamp   string          `json:"timestamp"`
	Level       Level           `json:"level"`
	Kind        entryKind       `json:"kind"`
	Command     string          `json:"command"`
	OperationID string          `json:"operationId"`
	Message     string          `json:"message"`
	Details     json.RawMessage `json:"details"`
}

type encodedEntry struct {
	line        []byte
	detailsJSON json.RawMessage
}

func encodeEntry(
	now time.Time,
	level Level,
	kind entryKind,
	command string,
	operationID string,
	message string,
	details map[string]any,
) (encodedEntry, error) {
	if !level.Valid() {
		return encodedEntry{}, ErrInvalidLevel
	}
	if now.IsZero() {
		return encodedEntry{}, ErrInvalidTime
	}
	if details == nil {
		details = map[string]any{}
	}

	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return encodedEntry{}, fmt.Errorf("%w: details: %w", ErrEncodeEntry, err)
	}
	wire := entry{
		Timestamp:   now.Format(time.RFC3339Nano),
		Level:       level,
		Kind:        kind,
		Command:     command,
		OperationID: operationID,
		Message:     message,
		Details:     json.RawMessage(detailsJSON),
	}
	line, err := json.Marshal(wire)
	if err != nil {
		return encodedEntry{}, fmt.Errorf("%w: entry: %w", ErrEncodeEntry, err)
	}
	line = append(line, '\n')
	return encodedEntry{
		line:        line,
		detailsJSON: json.RawMessage(detailsJSON),
	}, nil
}

func writeLine(writer io.Writer, line []byte) (bool, error) {
	n, writeErr := writer.Write(line)
	written := n > 0

	var shortErr error
	if n != len(line) {
		shortErr = fmt.Errorf("write log line: %w", io.ErrShortWrite)
	}
	if writeErr != nil {
		writeErr = fmt.Errorf("write log line: %w", writeErr)
	}
	return written, errors.Join(shortErr, writeErr)
}
