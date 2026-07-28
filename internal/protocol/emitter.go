package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Emitter is the serialized, flush-on-every-line outlet for NDJSON events.
type Emitter struct {
	mu           sync.Mutex
	writer       *bufio.Writer
	destination  io.Writer
	operationID  string
	nextSequence uint64
	clock        func() time.Time
	writeErr     error
}

type emitterOptions struct {
	operationID string
	clock       func() time.Time
}

// Option customizes an Emitter.
type Option func(*emitterOptions) error

// WithOperationID supplies an existing canonical ULID.
func WithOperationID(operationID string) Option {
	return func(options *emitterOptions) error {
		if !validOperationID(operationID) {
			return fmt.Errorf("invalid operation ID %q", operationID)
		}
		options.operationID = operationID
		return nil
	}
}

// WithClock supplies the event clock. It is primarily intended for tests.
func WithClock(clock func() time.Time) Option {
	return func(options *emitterOptions) error {
		if clock == nil {
			return errors.New("protocol clock must not be nil")
		}
		options.clock = clock
		return nil
	}
}

// NewEmitter creates an emitter and immediately writes the required first
// hello event.
func NewEmitter(
	output io.Writer,
	runtimeVersion string,
	command string,
	capabilities []string,
	options ...Option,
) (*Emitter, error) {
	if output == nil {
		return nil, errors.New("protocol output must not be nil")
	}

	settings := emitterOptions{clock: time.Now}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("protocol option must not be nil")
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}

	if settings.operationID == "" {
		operationID, err := newOperationID(settings.clock(), operationRandom)
		if err != nil {
			return nil, err
		}
		settings.operationID = operationID
	}

	emitter := &Emitter{
		writer:       bufio.NewWriter(output),
		destination:  output,
		operationID:  settings.operationID,
		nextSequence: 1,
		clock:        settings.clock,
	}

	if capabilities == nil {
		capabilities = []string{}
	}
	if err := emitter.emit(TypeHello, &HelloEvent{
		RuntimeVersion: runtimeVersion,
		Command:        command,
		Capabilities:   capabilities,
	}); err != nil {
		return nil, err
	}
	return emitter, nil
}

// OperationID returns the ULID shared by every event from this emitter.
func (e *Emitter) OperationID() string {
	return e.operationID
}

// EmitProgress emits a progress event.
func (e *Emitter) EmitProgress(event ProgressEvent) error {
	return e.emit(TypeProgress, &event)
}

// EmitState emits a lifecycle state event.
func (e *Emitter) EmitState(event StateEvent) error {
	return e.emit(TypeState, &event)
}

// EmitLog emits a managed-process log line.
func (e *Emitter) EmitLog(event LogEvent) error {
	return e.emit(TypeLog, &event)
}

// EmitWarning emits a non-terminal warning.
func (e *Emitter) EmitWarning(event WarningEvent) error {
	return e.emit(TypeWarning, &event)
}

// EmitError emits an operation error.
func (e *Emitter) EmitError(event ErrorEvent) error {
	return e.emit(TypeError, &event)
}

// EmitResult emits an operation result.
func (e *Emitter) EmitResult(event ResultEvent) error {
	return e.emit(TypeResult, &event)
}

func (e *Emitter) emit(eventType EventType, event eventWithCommon) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.writeErr != nil {
		return e.writeErr
	}

	event.setCommon(Common{
		Protocol:    Version,
		Type:        eventType,
		OperationID: e.operationID,
		Sequence:    e.nextSequence,
		Timestamp:   e.clock().Format(time.RFC3339Nano),
	})
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	encoded = append(encoded, '\n')
	if _, err := e.writer.Write(encoded); err != nil {
		return e.rememberWriteError(err)
	}
	if err := e.writer.Flush(); err != nil {
		return e.rememberWriteError(err)
	}
	if err := flushDestination(e.destination); err != nil {
		return e.rememberWriteError(err)
	}

	e.nextSequence++
	return nil
}

func (e *Emitter) rememberWriteError(err error) error {
	e.writeErr = fmt.Errorf("write protocol event: %w", err)
	return e.writeErr
}

type eventWithCommon interface {
	setCommon(Common)
}

func (e *HelloEvent) setCommon(common Common) {
	e.Common = common
}

func (e *ProgressEvent) setCommon(common Common) {
	e.Common = common
}

func (e *StateEvent) setCommon(common Common) {
	e.Common = common
}

func (e *LogEvent) setCommon(common Common) {
	e.Common = common
}

func (e *WarningEvent) setCommon(common Common) {
	e.Common = common
}

func (e *ErrorEvent) setCommon(common Common) {
	e.Common = common
}

func (e *ResultEvent) setCommon(common Common) {
	e.Common = common
}

type errorFlusher interface {
	Flush() error
}

type flusher interface {
	Flush()
}

func flushDestination(destination io.Writer) error {
	switch value := destination.(type) {
	case errorFlusher:
		return value.Flush()
	case flusher:
		value.Flush()
	}
	return nil
}
