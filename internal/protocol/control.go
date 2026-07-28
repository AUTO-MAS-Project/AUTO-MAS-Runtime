package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// ControlKind identifies a stdin control command.
type ControlKind string

const (
	ControlCancel   ControlKind = "cancel"
	ControlShutdown ControlKind = "shutdown"
	ControlStatus   ControlKind = "status"

	maxControlPayloadBytes  = 4096
	controlReaderBufferSize = maxControlPayloadBytes + 2
	invalidControlMessage   = "已忽略无效的 stdin 控制命令"
)

// ControlCommand is a versioned stdin control request.
type ControlCommand struct {
	Protocol  int         `json:"protocol"`
	Command   ControlKind `json:"command"`
	CommandID string      `json:"commandId"`
}

// ControlDisposition reports whether a prepared command can be accepted.
type ControlDisposition uint8

const (
	ControlDispositionUnknown ControlDisposition = iota
	ControlAccepted
	ControlNotApplicable
)

// ControlAction commits an accepted control command.
type ControlAction func() error

// ControlHandler prepares control commands without applying their side effects.
type ControlHandler interface {
	PrepareControl(ControlCommand) (ControlDisposition, ControlAction, error)
	CurrentControlStage() Stage
}

// ControlWarningEmitter emits warnings caused by invalid control input.
type ControlWarningEmitter interface {
	EmitWarning(WarningEvent) error
}

// ControlReader reads and dispatches newline-delimited stdin controls.
type ControlReader struct {
	input    *bufio.Reader
	warnings ControlWarningEmitter
	handler  ControlHandler
	allowed  map[ControlKind]struct{}
}

// NewControlReader constructs a bounded stdin control reader.
func NewControlReader(
	input io.Reader,
	warnings ControlWarningEmitter,
	handler ControlHandler,
	allowed ...ControlKind,
) (*ControlReader, error) {
	if interfaceIsNil(input) {
		return nil, fmt.Errorf("input must not be nil")
	}
	if interfaceIsNil(warnings) {
		return nil, fmt.Errorf("warning emitter must not be nil")
	}
	if interfaceIsNil(handler) {
		return nil, fmt.Errorf("control handler must not be nil")
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("at least one allowed control command is required")
	}

	allowedSet := make(map[ControlKind]struct{}, len(allowed))
	for _, command := range allowed {
		if !isKnownControlKind(command) {
			return nil, fmt.Errorf("unknown allowed control command %q", command)
		}
		if _, exists := allowedSet[command]; exists {
			return nil, fmt.Errorf("duplicate allowed control command %q", command)
		}
		allowedSet[command] = struct{}{}
	}

	return &ControlReader{
		input:    bufio.NewReaderSize(struct{ io.Reader }{Reader: input}, controlReaderBufferSize),
		warnings: warnings,
		handler:  handler,
		allowed:  allowedSet,
	}, nil
}

// Run reads controls synchronously until EOF or an infrastructure error.
func (r *ControlReader) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("control context must not be nil")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, lineBytes, tooLong, hasLine, reachedEOF, err := r.readPhysicalLine()
		if err != nil {
			return fmt.Errorf("read stdin control: %w", err)
		}
		if !hasLine {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if tooLong {
			if err := r.emitInvalidControl(map[string]any{
				"reason":    "line_too_long",
				"lineBytes": lineBytes,
			}); err != nil {
				return err
			}
		} else {
			command, invalidDetails := parseControlCommand(line)
			if invalidDetails != nil {
				if err := r.emitInvalidControl(invalidDetails); err != nil {
					return err
				}
			} else if err := r.dispatch(command); err != nil {
				return err
			}
		}

		if reachedEOF {
			return nil
		}
	}
}

func (r *ControlReader) readPhysicalLine() (
	line []byte,
	lineBytes int,
	tooLong bool,
	hasLine bool,
	reachedEOF bool,
	err error,
) {
	fragment, readErr := r.input.ReadSlice('\n')
	switch {
	case readErr == nil:
		line = fragment[:len(fragment)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return line, len(line), len(line) > maxControlPayloadBytes, true, false, nil
	case errors.Is(readErr, io.EOF):
		if len(fragment) == 0 {
			return nil, 0, false, false, true, nil
		}
		return fragment, len(fragment), len(fragment) > maxControlPayloadBytes, true, true, nil
	case errors.Is(readErr, bufio.ErrBufferFull):
		return r.drainOverlongLine(fragment)
	default:
		return nil, 0, false, false, false, readErr
	}
}

func (r *ControlReader) drainOverlongLine(first []byte) (
	line []byte,
	lineBytes int,
	tooLong bool,
	hasLine bool,
	reachedEOF bool,
	err error,
) {
	totalBytes := len(first)
	previousByte := first[len(first)-1]

	for {
		fragment, readErr := r.input.ReadSlice('\n')
		totalBytes += len(fragment)
		switch {
		case readErr == nil:
			payloadBytes := totalBytes - 1
			byteBeforeLF := previousByte
			if len(fragment) >= 2 {
				byteBeforeLF = fragment[len(fragment)-2]
			}
			if byteBeforeLF == '\r' {
				payloadBytes--
			}
			return nil, payloadBytes, true, true, false, nil
		case errors.Is(readErr, io.EOF):
			return nil, totalBytes, true, true, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			if len(fragment) > 0 {
				previousByte = fragment[len(fragment)-1]
			}
		default:
			return nil, 0, false, false, false, readErr
		}
	}
}

func (r *ControlReader) dispatch(command ControlCommand) error {
	if _, allowed := r.allowed[command.Command]; !allowed {
		return r.emitInvalidControl(WithControlCommandID(map[string]any{
			"reason":  "command_not_applicable",
			"command": command.Command,
		}, command.CommandID))
	}

	disposition, action, err := r.handler.PrepareControl(command)
	if err != nil {
		return fmt.Errorf("prepare stdin control %q: %w", command.Command, err)
	}
	switch disposition {
	case ControlAccepted:
		if action == nil {
			return fmt.Errorf("prepare stdin control %q: accepted command returned nil action", command.Command)
		}
		if err := action(); err != nil {
			return fmt.Errorf("apply stdin control %q: %w", command.Command, err)
		}
		return nil
	case ControlNotApplicable:
		if action != nil {
			return fmt.Errorf("prepare stdin control %q: not-applicable command returned an action", command.Command)
		}
		return r.emitInvalidControl(WithControlCommandID(map[string]any{
			"reason":  "command_not_applicable",
			"command": command.Command,
		}, command.CommandID))
	default:
		return fmt.Errorf("prepare stdin control %q: unknown disposition %d", command.Command, disposition)
	}
}

func (r *ControlReader) emitInvalidControl(details map[string]any) error {
	stage := r.handler.CurrentControlStage()
	if !IsKnownStage(stage) {
		return fmt.Errorf("emit invalid stdin control warning: unknown current stage %q", stage)
	}
	warning, err := NewWarningEvent(
		CodeInvalidControlCommand,
		stage,
		invalidControlMessage,
		details,
	)
	if err != nil {
		return fmt.Errorf("construct invalid stdin control warning: %w", err)
	}
	if err := r.warnings.EmitWarning(warning); err != nil {
		return fmt.Errorf("emit invalid stdin control warning: %w", err)
	}
	return nil
}

func parseControlCommand(line []byte) (ControlCommand, map[string]any) {
	if len(line) == 0 {
		return ControlCommand{}, map[string]any{
			"reason":    "empty_line",
			"lineBytes": 0,
		}
	}
	if !utf8.Valid(line) {
		return ControlCommand{}, map[string]any{
			"reason":    "invalid_utf8",
			"lineBytes": len(line),
		}
	}
	if !json.Valid(line) {
		return ControlCommand{}, map[string]any{
			"reason":    "invalid_json",
			"lineBytes": len(line),
		}
	}

	fields, invalidDetails := decodeControlFields(line)
	if invalidDetails != nil {
		return ControlCommand{}, invalidDetails
	}
	for _, field := range []string{"protocol", "command", "commandId"} {
		if _, exists := fields[field]; !exists {
			return ControlCommand{}, map[string]any{
				"reason": "missing_field",
				"field":  field,
			}
		}
	}

	var command ControlCommand
	if isJSONNull(fields["protocol"]) || json.Unmarshal(fields["protocol"], &command.Protocol) != nil {
		return ControlCommand{}, invalidFieldDetails("protocol")
	}
	if isJSONNull(fields["command"]) || json.Unmarshal(fields["command"], &command.Command) != nil {
		return ControlCommand{}, invalidFieldDetails("command")
	}
	if isJSONNull(fields["commandId"]) || json.Unmarshal(fields["commandId"], &command.CommandID) != nil {
		return ControlCommand{}, invalidFieldDetails("commandId")
	}

	if command.Protocol != Version {
		return ControlCommand{}, map[string]any{
			"reason":           "unsupported_protocol",
			"expectedProtocol": Version,
			"receivedProtocol": command.Protocol,
		}
	}
	if !validOperationID(command.CommandID) {
		return ControlCommand{}, map[string]any{
			"reason": "invalid_command_id",
			"field":  "commandId",
		}
	}
	if !isKnownControlKind(command.Command) {
		return ControlCommand{}, WithControlCommandID(map[string]any{
			"reason":  "unsupported_command",
			"command": command.Command,
		}, command.CommandID)
	}
	return command, nil
}

func decodeControlFields(line []byte) (map[string]json.RawMessage, map[string]any) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	first, err := decoder.Token()
	if err != nil {
		return nil, invalidJSONDetails(len(line))
	}
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, invalidJSONDetails(len(line))
	}

	fields := make(map[string]json.RawMessage, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalidJSONDetails(len(line))
		}
		field, ok := token.(string)
		if !ok {
			return nil, invalidJSONDetails(len(line))
		}
		if _, exists := fields[field]; exists {
			return nil, map[string]any{
				"reason": "duplicate_field",
				"field":  field,
			}
		}
		if !isControlField(field) {
			return nil, map[string]any{
				"reason": "unknown_field",
				"field":  field,
			}
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalidJSONDetails(len(line))
		}
		fields[field] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, invalidJSONDetails(len(line))
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, invalidJSONDetails(len(line))
	}
	return fields, nil
}

func invalidJSONDetails(lineBytes int) map[string]any {
	return map[string]any{
		"reason":    "invalid_json",
		"lineBytes": lineBytes,
	}
}

func invalidFieldDetails(field string) map[string]any {
	return map[string]any{
		"reason": "invalid_field",
		"field":  field,
	}
}

func isJSONNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func isControlField(field string) bool {
	switch field {
	case "protocol", "command", "commandId":
		return true
	default:
		return false
	}
}

func isKnownControlKind(command ControlKind) bool {
	switch command {
	case ControlCancel, ControlShutdown, ControlStatus:
		return true
	default:
		return false
	}
}

// WithControlCommandID copies details and associates them with a control command.
func WithControlCommandID(details map[string]any, commandID string) map[string]any {
	copied := make(map[string]any, len(details)+1)
	for key, value := range details {
		copied[key] = value
	}
	copied["controlCommandId"] = commandID
	return copied
}
