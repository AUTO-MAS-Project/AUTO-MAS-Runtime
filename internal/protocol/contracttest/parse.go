package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
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
	types := make([]string, len(events))
	for index, event := range events {
		value, ok := event.object["type"]
		switch value := value.(type) {
		case string:
			types[index] = value
		case nil:
			types[index] = "<missing>"
		default:
			if !ok {
				types[index] = "<missing>"
			} else {
				types[index] = fmt.Sprintf("<%T>", value)
			}
		}
	}
	return "[" + strings.Join(types, ",") + "]"
}

func quoteRaw(raw []byte) string {
	const maxRawBytes = 160
	if len(raw) <= maxRawBytes {
		return strconv.Quote(string(raw))
	}
	return strconv.Quote(string(raw[:maxRawBytes])) + "...(truncated)"
}
