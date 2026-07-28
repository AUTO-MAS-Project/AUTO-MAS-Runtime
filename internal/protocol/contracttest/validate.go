package contracttest

import (
	"encoding/json"
	"fmt"
	"reflect"
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

type schemaRule struct {
	field       string
	description string
	valid       func(any) bool
}

var resultSchema = []schemaRule{
	{field: "success", description: "a boolean", valid: isBoolean},
	{field: "code", description: "a string", valid: isString},
	{field: "stage", description: "a string", valid: isString},
	{field: "status", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
	{field: "retryable", description: "a boolean", valid: isBoolean},
	{field: "remediation", description: "an array of strings or null", valid: validRemediation},
	{field: "details", description: "an object or null", valid: validDetails},
}

var diagnosticSchema = []schemaRule{
	{field: "code", description: "a string", valid: isString},
	{field: "stage", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
	{field: "retryable", description: "a boolean", valid: isBoolean},
	{field: "remediation", description: "an array of strings or null", valid: validRemediation},
	{field: "details", description: "an object or null", valid: validDetails},
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

	issues = append(issues, validateBusinessSchemas(command, terminal, events)...)
	if helloCount != 1 {
		issues = append(issues, issueForTranscript(command, terminal, events, "hello must appear exactly once"))
	}
	if len(resultLines) != 1 {
		issues = append(issues, issueForTranscript(command, terminal, events, "result must appear exactly once"))
	} else if resultLines[0] != events[len(events)-1].line {
		resultEvent := eventAtPhysicalLine(events, resultLines[0])
		issues = append(issues, issueForEvent(command, terminal, resultEvent, "result must be the last event"))
	}
	if len(resultLines) == 1 {
		resultEvent := eventAtPhysicalLine(events, resultLines[0])
		issues = append(issues, validateTerminal(command, terminal, events, resultEvent)...)
		issues = append(issues, validateWarningSummary(command, terminal, events, resultEvent)...)
	}
	return issues
}

func validateBusinessSchemas(command string, terminal Terminal, events []parsedEvent) []contractIssue {
	var issues []contractIssue
	for _, event := range events {
		eventType, _ := event.object["type"].(string)
		switch eventType {
		case string(protocol.TypeResult):
			issues = append(issues, validateObjectSchema(command, terminal, event, "result", event.object, resultSchema)...)
			issues = append(issues, validateResultSummarySchemas(command, terminal, event)...)
		case string(protocol.TypeError):
			issues = append(issues, validateObjectSchema(command, terminal, event, "error", event.object, diagnosticSchema)...)
		case string(protocol.TypeWarning):
			issues = append(issues, validateObjectSchema(command, terminal, event, "warning", event.object, diagnosticSchema)...)
		}
	}
	return issues
}

func validateObjectSchema(
	command string,
	terminal Terminal,
	event parsedEvent,
	label string,
	object map[string]any,
	rules []schemaRule,
) []contractIssue {
	var issues []contractIssue
	for _, rule := range rules {
		value, exists := object[rule.field]
		if !exists || !rule.valid(value) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				event,
				fmt.Sprintf("%s field %q must be %s", label, rule.field, rule.description),
			))
		}
	}
	return issues
}

func validateResultSummarySchemas(
	command string,
	terminal Terminal,
	result parsedEvent,
) []contractIssue {
	details, ok := result.object["details"].(map[string]any)
	if !ok {
		return nil
	}
	value, exists := details["warnings"]
	if !exists {
		return nil
	}
	summaries, ok := value.([]any)
	if !ok {
		return nil
	}

	var issues []contractIssue
	for index, value := range summaries {
		summary, ok := value.(map[string]any)
		if !ok {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				result,
				fmt.Sprintf("result warning summary %d must be an object", index+1),
			))
			continue
		}
		label := fmt.Sprintf("result warning summary %d", index+1)
		issues = append(issues, validateObjectSchema(
			command,
			terminal,
			result,
			label,
			summary,
			diagnosticSchema,
		)...)
	}
	return issues
}

func validateTerminal(
	command string,
	terminal Terminal,
	events []parsedEvent,
	result parsedEvent,
) []contractIssue {
	var issues []contractIssue
	success, successOK := result.object["success"].(bool)
	code, codeOK := result.object["code"].(string)
	status, statusOK := result.object["status"].(string)
	retryable, retryableOK := result.object["retryable"].(bool)

	switch terminal {
	case TerminalSuccess:
		if !successOK || !success {
			issues = append(issues, issueForEvent(command, terminal, result, "success result must have success=true"))
		}
		if !codeOK || code != string(protocol.CodeOK) {
			issues = append(issues, issueForEvent(command, terminal, result, "success result code must be OK"))
		}
		if !retryableOK || retryable {
			issues = append(issues, issueForEvent(command, terminal, result, "success result must not be retryable"))
		}
		if !statusOK || status == "cancelled" {
			issues = append(issues, issueForEvent(command, terminal, result, "success result status must not be cancelled"))
		}
		if hasPriorEventType(events, result.line, string(protocol.TypeError)) {
			issues = append(issues, issueForEvent(command, terminal, result, "success transcript must not contain error before result"))
		}
	case TerminalFailure:
		if !successOK || success {
			issues = append(issues, issueForEvent(command, terminal, result, "failure result must have success=false"))
		}
		if !codeOK || code == string(protocol.CodeOK) || code == string(protocol.CodeOperationCancelled) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				result,
				"failure result code must not be OK or OPERATION_CANCELLED",
			))
		}
		if !statusOK || status == "cancelled" {
			issues = append(issues, issueForEvent(command, terminal, result, "failure result status must not be cancelled"))
		}
		if !hasMatchingPriorError(events, result) {
			issues = append(issues, issueForEvent(command, terminal, result, "failure result must match a prior error"))
		}
	case TerminalCancelled:
		if !successOK || success {
			issues = append(issues, issueForEvent(command, terminal, result, "cancelled result must have success=false"))
		}
		if !codeOK || code != string(protocol.CodeOperationCancelled) {
			issues = append(issues, issueForEvent(command, terminal, result, "cancelled result code must be OPERATION_CANCELLED"))
		}
		if !statusOK || status != "cancelled" {
			issues = append(issues, issueForEvent(command, terminal, result, "cancelled result status must be cancelled"))
		}
		if !hasMatchingPriorError(events, result) {
			issues = append(issues, issueForEvent(command, terminal, result, "cancelled result must match a prior error"))
		}
	default:
		issues = append(issues, issueForEvent(command, terminal, result, fmt.Sprintf("unknown terminal scenario %q", terminal)))
	}
	return issues
}

func validateWarningSummary(
	command string,
	terminal Terminal,
	events []parsedEvent,
	result parsedEvent,
) []contractIssue {
	warnings := priorEventsOfType(events, result.line, string(protocol.TypeWarning))
	details, detailsOK := result.object["details"].(map[string]any)
	if len(warnings) == 0 {
		if detailsOK && hasAnyWarningSummaryKey(details) {
			return []contractIssue{issueForEvent(
				command,
				terminal,
				result,
				"result warning summary keys must be absent when there are no warnings",
			)}
		}
		return nil
	}

	var issues []contractIssue
	if !detailsOK {
		return []contractIssue{issueForEvent(command, terminal, result, "result details must be an object when warnings exist")}
	}

	summariesValue, summariesPresent := details["warnings"]
	summaries, summariesOK := summariesValue.([]any)
	if !summariesPresent || !summariesOK {
		issues = append(issues, issueForEvent(command, terminal, result, "result warnings must be present as an array"))
	} else {
		expected := expectedWarningSummaries(warnings)
		if !reflect.DeepEqual(summaries, expected) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				result,
				"result warnings must equal the earliest warning events in order",
			))
		}
	}

	countValue, countPresent := details["warningCount"]
	if !countPresent {
		issues = append(issues, issueForEvent(command, terminal, result, "result warningCount must be present"))
	} else if count, ok := jsonUint64(countValue); !ok || count != uint64(len(warnings)) {
		issues = append(issues, issueForEvent(
			command,
			terminal,
			result,
			fmt.Sprintf("result warningCount must equal %d", len(warnings)),
		))
	}

	truncatedValue, truncatedPresent := details["warningsTruncated"]
	expectedTruncated := len(warnings) > protocol.MaxResultWarningSummaries
	if !truncatedPresent {
		issues = append(issues, issueForEvent(command, terminal, result, "result warningsTruncated must be present"))
	} else if truncated, ok := truncatedValue.(bool); !ok || truncated != expectedTruncated {
		issues = append(issues, issueForEvent(
			command,
			terminal,
			result,
			fmt.Sprintf("result warningsTruncated must equal %t", expectedTruncated),
		))
	}
	return issues
}

func hasPriorEventType(events []parsedEvent, resultLine int, eventType string) bool {
	return len(priorEventsOfType(events, resultLine, eventType)) != 0
}

func priorEventsOfType(events []parsedEvent, resultLine int, eventType string) []parsedEvent {
	var matches []parsedEvent
	for _, event := range events {
		if event.line >= resultLine {
			continue
		}
		if currentType, ok := event.object["type"].(string); ok && currentType == eventType {
			matches = append(matches, event)
		}
	}
	return matches
}

func hasMatchingPriorError(events []parsedEvent, result parsedEvent) bool {
	for _, event := range priorEventsOfType(events, result.line, string(protocol.TypeError)) {
		if tupleMatches(event.object, result.object) {
			return true
		}
	}
	return false
}

func tupleMatches(left map[string]any, right map[string]any) bool {
	leftCode, leftCodeOK := left["code"].(string)
	rightCode, rightCodeOK := right["code"].(string)
	leftStage, leftStageOK := left["stage"].(string)
	rightStage, rightStageOK := right["stage"].(string)
	leftRetryable, leftRetryableOK := left["retryable"].(bool)
	rightRetryable, rightRetryableOK := right["retryable"].(bool)
	leftRemediation, leftRemediationOK := left["remediation"]
	rightRemediation, rightRemediationOK := right["remediation"]
	if !leftCodeOK || !rightCodeOK ||
		!leftStageOK || !rightStageOK ||
		!leftRetryableOK || !rightRetryableOK ||
		!leftRemediationOK || !rightRemediationOK ||
		!validRemediation(leftRemediation) || !validRemediation(rightRemediation) {
		return false
	}
	return leftCode == rightCode &&
		leftStage == rightStage &&
		leftRetryable == rightRetryable &&
		reflect.DeepEqual(leftRemediation, rightRemediation)
}

func validRemediation(value any) bool {
	if value == nil {
		return true
	}
	remediation, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range remediation {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func validDetails(value any) bool {
	if value == nil {
		return true
	}
	_, ok := value.(map[string]any)
	return ok
}

func isString(value any) bool {
	_, ok := value.(string)
	return ok
}

func isBoolean(value any) bool {
	_, ok := value.(bool)
	return ok
}

func expectedWarningSummaries(warnings []parsedEvent) []any {
	count := len(warnings)
	if count > protocol.MaxResultWarningSummaries {
		count = protocol.MaxResultWarningSummaries
	}
	summaries := make([]any, count)
	for index := 0; index < count; index++ {
		summary := make(map[string]any, 6)
		for _, field := range []string{"code", "stage", "message", "retryable", "remediation", "details"} {
			summary[field] = warnings[index].object[field]
		}
		summaries[index] = summary
	}
	return summaries
}

func hasAnyWarningSummaryKey(details map[string]any) bool {
	for _, key := range []string{"warnings", "warningCount", "warningsTruncated"} {
		if _, exists := details[key]; exists {
			return true
		}
	}
	return false
}

func jsonUint64(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	count, err := strconv.ParseUint(number.String(), 10, 64)
	return count, err == nil
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
