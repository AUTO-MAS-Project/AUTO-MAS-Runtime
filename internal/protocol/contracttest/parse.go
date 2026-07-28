package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxTypeLabelBytes          = 64
	maxTypeSummaryBytes        = 256
	diagnosticTruncationMarker = "...(truncated)"
)

type parsedEvent struct {
	line   int
	raw    []byte
	object map[string]any
}

type contractIssue struct {
	command  string
	scenario Terminal
	line     int
	raw      []byte
	types    string
	message  string
}

func (i contractIssue) Error() string {
	return fmt.Sprintf(
		"contract violation: command=%q scenario=%q line=%d raw=%s types=%s: %s",
		i.command,
		i.scenario,
		i.line,
		quoteRaw(i.raw),
		i.types,
		i.message,
	)
}

func inspect(command string, terminal Terminal, stdout []byte) ([]parsedEvent, []contractIssue) {
	events, issues := parsePhysicalLines(command, terminal, stdout)
	issues = append(issues, validateEnvelope(command, terminal, events)...)

	summary := summarizeTypes(events)
	for index := range issues {
		issues[index].types = summary
	}
	return events, issues
}

func parsePhysicalLines(command string, terminal Terminal, stdout []byte) ([]parsedEvent, []contractIssue) {
	if len(stdout) == 0 {
		return nil, []contractIssue{newIssue(command, terminal, 0, nil, "transcript is empty")}
	}

	var issues []contractIssue
	if stdout[len(stdout)-1] != '\n' {
		lastLF := bytes.LastIndexByte(stdout, '\n')
		lineNumber := bytes.Count(stdout, []byte{'\n'}) + 1
		issues = append(issues, newIssue(
			command,
			terminal,
			lineNumber,
			stdout[lastLF+1:],
			"transcript must end with LF",
		))
	}

	content := stdout
	if content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	lines := bytes.Split(content, []byte{'\n'})
	events := make([]parsedEvent, 0, len(lines))
	for index, line := range lines {
		lineNumber := index + 1
		if len(line) == 0 {
			issues = append(issues, newIssue(command, terminal, lineNumber, line, "empty physical line"))
			continue
		}
		if bytes.ContainsRune(line, '\r') {
			issues = append(issues, newIssue(command, terminal, lineNumber, line, "physical line contains CR"))
			continue
		}
		if !utf8.Valid(line) {
			issues = append(issues, newIssue(command, terminal, lineNumber, line, "physical line contains invalid UTF-8"))
			continue
		}
		if name, duplicate := duplicateJSONObjectName(line); duplicate {
			issues = append(issues, newIssue(
				command,
				terminal,
				lineNumber,
				line,
				fmt.Sprintf("duplicate JSON object name %q", name),
			))
			continue
		}

		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			issues = append(issues, newIssue(command, terminal, lineNumber, line, fmt.Sprintf("invalid JSON: %v", err)))
			continue
		}

		var trailing any
		err := decoder.Decode(&trailing)
		switch {
		case err == nil:
			issues = append(issues, newIssue(command, terminal, lineNumber, line, "physical line contains a trailing JSON value"))
			continue
		case err != io.EOF:
			issues = append(issues, newIssue(command, terminal, lineNumber, line, fmt.Sprintf("physical line contains a trailing token: %v", err)))
			continue
		}

		object, ok := value.(map[string]any)
		if !ok {
			issues = append(issues, newIssue(command, terminal, lineNumber, line, "physical line must be a JSON object"))
			continue
		}
		events = append(events, parsedEvent{
			line:   lineNumber,
			raw:    append([]byte(nil), line...),
			object: object,
		})
	}
	return events, issues
}

func duplicateJSONObjectName(line []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	name, duplicate, err := scanJSONValue(decoder)
	if err != nil {
		return "", false
	}
	return name, duplicate
}

func scanJSONValue(decoder *json.Decoder) (string, bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", false, err
	}
	delimiter, structured := token.(json.Delim)
	if !structured {
		return "", false, nil
	}

	switch delimiter {
	case '{':
		names := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return "", false, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return "", false, fmt.Errorf("JSON object name has type %T", keyToken)
			}
			if _, exists := names[key]; exists {
				return key, true, nil
			}
			names[key] = struct{}{}
			if name, duplicate, err := scanJSONValue(decoder); err != nil || duplicate {
				return name, duplicate, err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return "", false, err
		}
	case '[':
		for decoder.More() {
			if name, duplicate, err := scanJSONValue(decoder); err != nil || duplicate {
				return name, duplicate, err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return "", false, err
		}
	default:
		return "", false, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return "", false, nil
}

func newIssue(command string, terminal Terminal, line int, raw []byte, message string) contractIssue {
	return contractIssue{
		command:  command,
		scenario: terminal,
		line:     line,
		raw:      append([]byte(nil), raw...),
		message:  message,
	}
}

func summarizeTypes(events []parsedEvent) string {
	if len(events) == 0 {
		return "[]"
	}

	var summary strings.Builder
	summary.Grow(maxTypeSummaryBytes)
	summary.WriteByte('[')
	for index, event := range events {
		label := summarizeEventType(event)
		separatorLength := 0
		if index != 0 {
			separatorLength = 1
		}
		required := separatorLength + len(label) + 1
		if index != len(events)-1 {
			required += 1 + len(diagnosticTruncationMarker)
		}
		if summary.Len()+required > maxTypeSummaryBytes {
			if index != 0 {
				summary.WriteByte(',')
			}
			summary.WriteString(diagnosticTruncationMarker)
			summary.WriteByte(']')
			return summary.String()
		}
		if index != 0 {
			summary.WriteByte(',')
		}
		summary.WriteString(label)
	}
	summary.WriteByte(']')
	return summary.String()
}

func summarizeEventType(event parsedEvent) string {
	value, exists := event.object["type"]
	switch value := value.(type) {
	case string:
		return boundedTypeName(value)
	case nil:
		return "<missing>"
	default:
		if !exists {
			return "<missing>"
		}
		return fmt.Sprintf("<%T>", value)
	}
}

func boundedTypeName(value string) string {
	return truncateUTF8String(value, maxTypeLabelBytes)
}

func truncateUTF8String(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	prefixLimit := maxBytes - len(diagnosticTruncationMarker)
	prefix := validUTF8Prefix([]byte(value), prefixLimit)
	return string(prefix) + diagnosticTruncationMarker
}

func validUTF8Prefix(value []byte, maxBytes int) []byte {
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.Valid(value[:end]) {
		end--
	}
	return value[:end]
}

func quoteRaw(raw []byte) string {
	const maxRawBytes = 160
	if len(raw) <= maxRawBytes {
		return strconv.Quote(string(raw))
	}
	prefix := raw[:maxRawBytes]
	if utf8.Valid(raw) {
		prefix = validUTF8Prefix(raw, maxRawBytes)
	}
	return strconv.Quote(string(prefix)) + diagnosticTruncationMarker
}
