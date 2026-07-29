package contracttest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type schemaRule struct {
	field       string
	description string
	valid       func(any) bool
	optional    bool
}

var helloSchema = []schemaRule{
	{field: "runtimeVersion", description: "a string", valid: isString},
	{field: "command", description: "a string", valid: isString},
	{field: "capabilities", description: "an array of strings", valid: validStringArray},
}

var progressSchema = []schemaRule{
	{field: "stage", description: "a string", valid: isString},
	{field: "status", description: "a string", valid: isString},
	{field: "current", description: "an integer when present", valid: isInteger, optional: true},
	{field: "total", description: "an integer when present", valid: isInteger, optional: true},
	{field: "percent", description: "a number when present", valid: isNumber, optional: true},
	{field: "message", description: "a string", valid: isString},
}

var stateSchema = []schemaRule{
	{field: "stage", description: "a string", valid: isString},
	{field: "status", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
	{field: "details", description: "an object", valid: validDetails},
}

var logSchema = []schemaRule{
	{field: "source", description: "a string", valid: isString},
	{field: "stream", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
}

var resultSchema = []schemaRule{
	{field: "success", description: "a boolean", valid: isBoolean},
	{field: "code", description: "a string", valid: isString},
	{field: "stage", description: "a string", valid: isString},
	{field: "status", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
	{field: "retryable", description: "a boolean", valid: isBoolean},
	{field: "remediation", description: "an array of strings", valid: validRemediation},
	{field: "details", description: "an object", valid: validDetails},
}

var diagnosticSchema = []schemaRule{
	{field: "code", description: "a string", valid: isString},
	{field: "stage", description: "a string", valid: isString},
	{field: "message", description: "a string", valid: isString},
	{field: "retryable", description: "a boolean", valid: isBoolean},
	{field: "remediation", description: "an array of strings", valid: validRemediation},
	{field: "details", description: "an object", valid: validDetails},
}

// validateBusinessSchemas 按事件类型应用必填字段、可选字段和稳定值 schema。
func validateBusinessSchemas(command string, terminal Terminal, events []parsedEvent) []contractIssue {
	var issues []contractIssue
	for _, event := range events {
		eventType, _ := event.object["type"].(string)
		switch eventType {
		case string(protocol.TypeHello):
			issues = append(issues, validateObjectSchema(command, terminal, event, "hello", event.object, helloSchema)...)
		case string(protocol.TypeProgress):
			issues = append(issues, validateObjectSchema(command, terminal, event, "progress", event.object, progressSchema)...)
		case string(protocol.TypeState):
			issues = append(issues, validateObjectSchema(command, terminal, event, "state", event.object, stateSchema)...)
		case string(protocol.TypeLog):
			issues = append(issues, validateObjectSchema(command, terminal, event, "log", event.object, logSchema)...)
		case string(protocol.TypeResult):
			issues = append(issues, validateObjectSchema(command, terminal, event, "result", event.object, resultSchema)...)
			issues = append(issues, validateResultSummarySchemas(command, terminal, event)...)
		case string(protocol.TypeError):
			issues = append(issues, validateObjectSchema(command, terminal, event, "error", event.object, diagnosticSchema)...)
		case string(protocol.TypeWarning):
			issues = append(issues, validateObjectSchema(command, terminal, event, "warning", event.object, diagnosticSchema)...)
		}
		issues = append(issues, validateStableValues(command, terminal, event)...)
		issues = append(issues, validateErrorDefinitions(command, terminal, event)...)
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
		if !exists && rule.optional {
			continue
		}
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

func validateStableValues(command string, terminal Terminal, event parsedEvent) []contractIssue {
	eventType, _ := event.object["type"].(string)
	var issues []contractIssue
	switch eventType {
	case string(protocol.TypeHello):
		capabilities, ok := event.object["capabilities"].([]any)
		if !ok {
			return nil
		}
		for _, value := range capabilities {
			capability, ok := value.(string)
			if ok && !protocol.IsKnownCapability(protocol.Capability(capability)) {
				issues = append(issues, issueForEvent(
					command,
					terminal,
					event,
					fmt.Sprintf("hello capability %q is not stable", boundedStableValue(capability)),
				))
			}
		}
	case string(protocol.TypeProgress):
		issues = append(issues, validateStableStage(command, terminal, event, "progress")...)
		if status, ok := event.object["status"].(string); ok &&
			!protocol.IsKnownProgressStatus(protocol.ProgressStatus(status)) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				event,
				fmt.Sprintf("progress status %q is not stable", boundedStableValue(status)),
			))
		}
	case string(protocol.TypeState):
		issues = append(issues, validateStableStage(command, terminal, event, "state")...)
		if status, ok := event.object["status"].(string); ok &&
			!protocol.IsKnownStateStatus(protocol.StateStatus(status)) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				event,
				fmt.Sprintf("state status %q is not stable", boundedStableValue(status)),
			))
		}
	case string(protocol.TypeWarning), string(protocol.TypeError), string(protocol.TypeResult):
		issues = append(issues, validateStableStage(command, terminal, event, eventType)...)
		issues = append(issues, validateStableCode(command, terminal, event, eventType)...)
		issues = append(issues, validateStableRemediations(command, terminal, event, eventType)...)
	}
	return issues
}

func validateStableStage(
	command string,
	terminal Terminal,
	event parsedEvent,
	label string,
) []contractIssue {
	stage, ok := event.object["stage"].(string)
	if !ok || protocol.IsKnownStage(protocol.Stage(stage)) {
		return nil
	}
	return []contractIssue{issueForEvent(
		command,
		terminal,
		event,
		fmt.Sprintf("%s stage %q is not stable", label, boundedStableValue(stage)),
	)}
}

func validateStableCode(
	command string,
	terminal Terminal,
	event parsedEvent,
	label string,
) []contractIssue {
	code, ok := event.object["code"].(string)
	if !ok || protocol.IsKnownCode(protocol.Code(code)) {
		return nil
	}
	return []contractIssue{issueForEvent(
		command,
		terminal,
		event,
		fmt.Sprintf("%s code %q is not stable", label, boundedStableValue(code)),
	)}
}

func validateStableRemediations(
	command string,
	terminal Terminal,
	event parsedEvent,
	label string,
) []contractIssue {
	values, ok := event.object["remediation"].([]any)
	if !ok {
		return nil
	}

	var issues []contractIssue
	for _, value := range values {
		remediation, ok := value.(string)
		if ok && !protocol.IsKnownRemediation(protocol.Remediation(remediation)) {
			issues = append(issues, issueForEvent(
				command,
				terminal,
				event,
				fmt.Sprintf("%s remediation %q is not stable", label, boundedStableValue(remediation)),
			))
		}
	}
	return issues
}

func validateErrorDefinitions(
	command string,
	terminal Terminal,
	event parsedEvent,
) []contractIssue {
	eventType, _ := event.object["type"].(string)
	switch eventType {
	case string(protocol.TypeWarning), string(protocol.TypeError):
		return validateErrorDefinition(command, terminal, event, eventType)
	case string(protocol.TypeResult):
		success, ok := event.object["success"].(bool)
		if !ok {
			return nil
		}
		if !success {
			return validateErrorDefinition(command, terminal, event, "result")
		}
		remediation, ok := event.object["remediation"].([]any)
		if ok && len(remediation) != 0 {
			return []contractIssue{issueForEvent(
				command,
				terminal,
				event,
				"success result remediation must be empty",
			)}
		}
	}
	return nil
}

func validateErrorDefinition(
	command string,
	terminal Terminal,
	event parsedEvent,
	label string,
) []contractIssue {
	code, ok := event.object["code"].(string)
	if !ok {
		return nil
	}
	definition, found := protocol.LookupErrorDefinition(protocol.Code(code))
	if !found {
		if protocol.IsKnownCode(protocol.Code(code)) {
			return []contractIssue{issueForEvent(
				command,
				terminal,
				event,
				fmt.Sprintf("%s code %q has no error definition", label, code),
			)}
		}
		return nil
	}

	var issues []contractIssue
	if label != "warning" && definition.ExitCode == protocol.ExitCodeSuccess {
		issues = append(issues, issueForEvent(
			command,
			terminal,
			event,
			fmt.Sprintf("%s code %q is warning-only", label, code),
		))
	}
	if retryable, ok := event.object["retryable"].(bool); ok && retryable != definition.Retryable {
		issues = append(issues, issueForEvent(
			command,
			terminal,
			event,
			fmt.Sprintf("%s retryable must match definition for code %q", label, code),
		))
	}
	if remediation, ok := remediationValues(event.object["remediation"]); ok &&
		!reflect.DeepEqual(remediation, definition.Remediation) {
		issues = append(issues, issueForEvent(
			command,
			terminal,
			event,
			fmt.Sprintf("%s remediation must match definition for code %q", label, code),
		))
	}
	return issues
}

func remediationValues(value any) ([]protocol.Remediation, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	remediations := make([]protocol.Remediation, len(values))
	for index, value := range values {
		remediation, ok := value.(string)
		if !ok {
			return nil, false
		}
		remediations[index] = protocol.Remediation(remediation)
	}
	return remediations, true
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

func validRemediation(value any) bool {
	return validStringArray(value)
}

func validStringArray(value any) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range values {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func validDetails(value any) bool {
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

func isInteger(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	_, err := strconv.ParseInt(number.String(), 10, 64)
	return err == nil
}

func isNumber(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	_, err := number.Float64()
	return err == nil
}

func boundedStableValue(value string) string {
	return truncateUTF8String(value, maxDiagnosticValueBytes)
}
