package contracttest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var canonicalULIDPattern = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

var knownEventTypes = map[string]struct{}{
	string(protocol.TypeHello):    {},
	string(protocol.TypeProgress): {},
	string(protocol.TypeState):    {},
	string(protocol.TypeLog):      {},
	string(protocol.TypeWarning):  {},
	string(protocol.TypeError):    {},
	string(protocol.TypeResult):   {},
}

func validateEnvelope(command string, terminal Terminal, events []parsedEvent) []contractIssue {
	var issues []contractIssue
	helloCount := 0
	resultLines := make([]int, 0, 1)
	var operationID string

	for index, event := range events {
		eventType, typeOK := event.object["type"].(string)
		if !typeOK {
			issues = append(issues, issueForEvent(command, terminal, event, "type must be a string"))
		} else {
			if _, known := knownEventTypes[eventType]; !known {
				issues = append(issues, issueForEvent(command, terminal, event, fmt.Sprintf("unknown event type %q", eventType)))
			}
			if eventType == string(protocol.TypeHello) {
				helloCount++
			}
			if eventType == string(protocol.TypeResult) {
				resultLines = append(resultLines, event.line)
			}
		}

		if index == 0 && eventType != string(protocol.TypeHello) {
			issues = append(issues, issueForEvent(command, terminal, event, "first event must be hello"))
		}

		if number, ok := event.object["protocol"].(json.Number); !ok || number.String() != strconv.Itoa(protocol.Version) {
			issues = append(issues, issueForEvent(command, terminal, event, fmt.Sprintf("protocol must equal %d", protocol.Version)))
		}

		currentOperationID, ok := event.object["operationId"].(string)
		if !ok || !canonicalULIDPattern.MatchString(currentOperationID) {
			issues = append(issues, issueForEvent(command, terminal, event, "operationId must be a canonical ULID"))
		}
		if index == 0 {
			operationID = currentOperationID
		} else if currentOperationID != operationID {
			issues = append(issues, issueForEvent(command, terminal, event, "operationId must match the first event"))
		}

		validateSequence(command, terminal, event, &issues)

		timestamp, ok := event.object["timestamp"].(string)
		if !ok {
			issues = append(issues, issueForEvent(command, terminal, event, "timestamp must be RFC3339Nano"))
		} else if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
			issues = append(issues, issueForEvent(command, terminal, event, "timestamp must be RFC3339Nano"))
		}
	}

	if helloCount != 1 {
		issues = append(issues, issueForTranscript(command, terminal, events, "hello must appear exactly once"))
	}
	if len(resultLines) != 1 {
		issues = append(issues, issueForTranscript(command, terminal, events, "result must appear exactly once"))
	} else if resultLines[0] != len(events) {
		resultEvent := eventAtPhysicalLine(events, resultLines[0])
		issues = append(issues, issueForEvent(command, terminal, resultEvent, "result must be the last event"))
	}
	return issues
}

func validateSequence(command string, terminal Terminal, event parsedEvent, issues *[]contractIssue) {
	number, ok := event.object["sequence"].(json.Number)
	if !ok {
		*issues = append(*issues, issueForEvent(command, terminal, event, "sequence must be an integer"))
		return
	}
	value, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil {
		*issues = append(*issues, issueForEvent(command, terminal, event, "sequence must be an integer"))
		return
	}
	if value != uint64(event.line) {
		*issues = append(*issues, issueForEvent(
			command,
			terminal,
			event,
			fmt.Sprintf("sequence must equal physical line %d", event.line),
		))
	}
}

func issueForEvent(command string, terminal Terminal, event parsedEvent, message string) contractIssue {
	return newIssue(command, terminal, event.line, event.raw, message)
}

func issueForTranscript(command string, terminal Terminal, events []parsedEvent, message string) contractIssue {
	if len(events) == 0 {
		return newIssue(command, terminal, 0, nil, message)
	}
	last := events[len(events)-1]
	return issueForEvent(command, terminal, last, message)
}

func eventAtPhysicalLine(events []parsedEvent, line int) parsedEvent {
	for _, event := range events {
		if event.line == line {
			return event
		}
	}
	return parsedEvent{line: line}
}
