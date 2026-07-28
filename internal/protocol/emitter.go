package protocol

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ProcessOutput owns the process-wide event renderer, sequence, serialization
// lock, and single-emitter reservation.
type ProcessOutput struct {
	mu             sync.Mutex
	renderer       EventRenderer
	nextSequence   uint64
	writeErr       error
	emitterCreated bool
}

// Emitter writes typed protocol events through a ProcessOutput.
type Emitter struct {
	output      *ProcessOutput
	operationID string
	clock       func() time.Time
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

// NewProcessOutput creates the process-wide owner of the NDJSON destination.
func NewProcessOutput(output io.Writer) (*ProcessOutput, error) {
	renderer, err := NewNDJSONRenderer(output)
	if err != nil {
		return nil, err
	}
	return NewProcessOutputWithRenderer(renderer)
}

// NewProcessOutputWithRenderer creates a ProcessOutput whose events are
// projected by renderer instead of written to an NDJSON destination.
func NewProcessOutputWithRenderer(renderer EventRenderer) (*ProcessOutput, error) {
	if interfaceIsNil(renderer) {
		return nil, errors.New("protocol renderer must not be nil")
	}
	return &ProcessOutput{renderer: renderer, nextSequence: 1}, nil
}

// NewEmitter reserves this process output for one operation and immediately
// writes the required first hello event. A ProcessOutput cannot create a
// second emitter.
func (o *ProcessOutput) NewEmitter(
	runtimeVersion string,
	command string,
	capabilities []string,
	options ...Option,
) (*Emitter, error) {
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

	o.mu.Lock()
	if o.emitterCreated {
		o.mu.Unlock()
		return nil, errors.New("protocol process output already has an emitter")
	}
	o.emitterCreated = true
	o.mu.Unlock()

	emitter := &Emitter{
		output:      o,
		operationID: settings.operationID,
		clock:       settings.clock,
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
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if e.output.writeErr != nil {
		return e.output.writeErr
	}

	event.setCommon(Common{
		Protocol:    Version,
		Type:        eventType,
		OperationID: e.operationID,
		Sequence:    e.output.nextSequence,
		Timestamp:   e.clock().Format(time.RFC3339Nano),
	})
	if err := renderEvent(e.output.renderer, event); err != nil {
		var nonSticky *nonStickyRenderError
		if errors.As(err, &nonSticky) {
			return nonSticky.err
		}
		return e.output.rememberWriteError(err)
	}

	e.output.nextSequence++
	return nil
}

func (o *ProcessOutput) rememberWriteError(err error) error {
	o.writeErr = fmt.Errorf("write protocol event: %w", err)
	return o.writeErr
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
