package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	// ErrResultAlreadyEmitted reports an attempt to emit a second terminal result.
	ErrResultAlreadyEmitted = errors.New("protocol result already emitted")
	// ErrEventAfterResult reports an attempt to emit a non-result event after the terminal result.
	ErrEventAfterResult = errors.New("protocol event emitted after result")
)

// ProcessOutput owns the process-wide event renderer, sequence, serialization
// lock, and single-emitter reservation.
type ProcessOutput struct {
	mu             sync.Mutex
	renderer       EventRenderer
	nextSequence   uint64
	writeErr       error
	emitterCreated bool
	terminal       bool
	warnings       []WarningSummary
	warningCount   uint64
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
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if err := e.output.rejectLocked(TypeWarning); err != nil {
		return err
	}
	canonical, ledger, err := snapshotWarning(event)
	if err != nil {
		return err
	}
	if err := e.emitPreparedLocked(TypeWarning, &canonical); err != nil {
		return err
	}
	e.output.warningCount++
	if len(e.output.warnings) < MaxResultWarningSummaries {
		e.output.warnings = append(e.output.warnings, ledger)
	}
	return nil
}

// EmitError emits an operation error.
func (e *Emitter) EmitError(event ErrorEvent) error {
	return e.emit(TypeError, &event)
}

// EmitResult emits an operation result.
func (e *Emitter) EmitResult(event ResultEvent) error {
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if err := e.output.rejectLocked(TypeResult); err != nil {
		return err
	}
	event.Details = e.output.resultDetails(event.Details)
	return e.emitPreparedLocked(TypeResult, &event)
}

func (e *Emitter) emit(eventType EventType, event eventWithCommon) error {
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if err := e.output.rejectLocked(eventType); err != nil {
		return err
	}
	return e.emitPreparedLocked(eventType, event)
}

func (o *ProcessOutput) rejectLocked(eventType EventType) error {
	if o.terminal {
		if eventType == TypeResult {
			return ErrResultAlreadyEmitted
		}
		return ErrEventAfterResult
	}
	if o.writeErr != nil {
		return o.writeErr
	}
	return nil
}

func (e *Emitter) emitPreparedLocked(eventType EventType, event eventWithCommon) error {
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
	if eventType == TypeResult {
		e.output.terminal = true
	}
	return nil
}

func snapshotWarning(event WarningEvent) (WarningEvent, WarningSummary, error) {
	businessFields := WarningSummary{
		Code:        event.Code,
		Stage:       event.Stage,
		Message:     event.Message,
		Retryable:   event.Retryable,
		Remediation: event.Remediation,
		Details:     event.Details,
	}
	encoded, err := json.Marshal(businessFields)
	if err != nil {
		return WarningEvent{}, WarningSummary{}, fmt.Errorf("encode warning event: %w", err)
	}
	canonicalSummary, err := decodeWarningSummary(encoded)
	if err != nil {
		return WarningEvent{}, WarningSummary{}, fmt.Errorf("decode canonical warning snapshot: %w", err)
	}
	ledgerSummary, err := decodeWarningSummary(encoded)
	if err != nil {
		return WarningEvent{}, WarningSummary{}, fmt.Errorf("decode warning ledger snapshot: %w", err)
	}
	canonical := WarningEvent{
		Code:        canonicalSummary.Code,
		Stage:       canonicalSummary.Stage,
		Message:     canonicalSummary.Message,
		Retryable:   canonicalSummary.Retryable,
		Remediation: canonicalSummary.Remediation,
		Details:     canonicalSummary.Details,
	}
	return canonical, ledgerSummary, nil
}

func decodeWarningSummary(encoded []byte) (WarningSummary, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var summary WarningSummary
	if err := decoder.Decode(&summary); err != nil {
		return WarningSummary{}, err
	}
	return summary, nil
}

func (o *ProcessOutput) resultDetails(details map[string]any) map[string]any {
	if details == nil && o.warningCount == 0 {
		return nil
	}
	copied := make(map[string]any, len(details)+3)
	for key, value := range details {
		copied[key] = value
	}
	delete(copied, "warnings")
	delete(copied, "warningCount")
	delete(copied, "warningsTruncated")
	if o.warningCount == 0 {
		return copied
	}

	summaries := make([]WarningSummary, len(o.warnings))
	for index, warning := range o.warnings {
		summaries[index] = cloneWarningSummary(warning)
	}
	copied["warnings"] = summaries
	copied["warningCount"] = o.warningCount
	copied["warningsTruncated"] = o.warningCount > MaxResultWarningSummaries
	return copied
}

func cloneWarningSummary(summary WarningSummary) WarningSummary {
	cloned := summary
	if summary.Remediation != nil {
		cloned.Remediation = append([]string{}, summary.Remediation...)
	}
	if summary.Details != nil {
		cloned.Details = cloneJSONObject(summary.Details)
	}
	return cloned
}

func cloneJSONObject(object map[string]any) map[string]any {
	cloned := make(map[string]any, len(object))
	for key, value := range object {
		cloned[key] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneJSONObject(value)
	case []any:
		cloned := make([]any, len(value))
		for index, item := range value {
			cloned[index] = cloneJSONValue(item)
		}
		return cloned
	default:
		return value
	}
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
