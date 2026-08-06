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
	// ErrResultAlreadyEmitted 表示调用方试图发射第二个终态 result。
	ErrResultAlreadyEmitted = errors.New("protocol result already emitted")
	// ErrOutputWriteFailed 表示 renderer 已无法把协议事件写到目标输出。
	ErrOutputWriteFailed = errors.New("protocol output write failed")
	// ErrEventAfterResult 表示调用方试图在终态 result 后继续发射非 result 事件。
	ErrEventAfterResult = errors.New("protocol event emitted after result")
)

// ProcessOutput 独占进程级事件 renderer、sequence、串行化锁与单 Emitter 预约。
type ProcessOutput struct {
	// mu 线性化 renderer 写入、sequence 分配、终态、写错误、Emitter 预约和 warning 账本。
	mu             sync.Mutex
	renderer       EventRenderer
	nextSequence   uint64
	writeErr       error
	emitterCreated bool
	terminal       bool
	warnings       []WarningSummary
	warningCount   uint64
}

// outputWriteError 保留既有诊断文本，同时让调用方能稳定识别输出通道故障。
type outputWriteError struct {
	cause error
}

func (e *outputWriteError) Error() string {
	return "write protocol event: " + e.cause.Error()
}

func (e *outputWriteError) Unwrap() []error {
	return []error{ErrOutputWriteFailed, e.cause}
}

// Emitter 通过 ProcessOutput 写出类型化协议事件。
type Emitter struct {
	output      *ProcessOutput
	operationID string
	clock       func() time.Time
}

type emitterOptions struct {
	operationID string
	clock       func() time.Time
}

// Option 配置 Emitter。
type Option func(*emitterOptions) error

// WithOperationID 注入已有的规范 ULID。
func WithOperationID(operationID string) Option {
	return func(options *emitterOptions) error {
		if !validOperationID(operationID) {
			return fmt.Errorf("invalid operation ID %q", operationID)
		}
		options.operationID = operationID
		return nil
	}
}

// WithClock 注入事件时钟，主要供测试使用。
func WithClock(clock func() time.Time) Option {
	return func(options *emitterOptions) error {
		if clock == nil {
			return errors.New("protocol clock must not be nil")
		}
		options.clock = clock
		return nil
	}
}

// NewProcessOutput 创建进程内唯一的 NDJSON 输出所有者。
func NewProcessOutput(output io.Writer) (*ProcessOutput, error) {
	renderer, err := NewNDJSONRenderer(output)
	if err != nil {
		return nil, err
	}
	return NewProcessOutputWithRenderer(renderer)
}

// NewProcessOutputWithRenderer 创建由 renderer 投影事件而非直接写 NDJSON 的 ProcessOutput。
func NewProcessOutputWithRenderer(renderer EventRenderer) (*ProcessOutput, error) {
	if interfaceIsNil(renderer) {
		return nil, errors.New("protocol renderer must not be nil")
	}
	return &ProcessOutput{renderer: renderer, nextSequence: 1}, nil
}

// NewEmitter 为单次操作预约此 ProcessOutput，并立即写出必须位于首位的 hello 事件。
// 同一个 ProcessOutput 不能创建第二个 Emitter。
func (o *ProcessOutput) NewEmitter(
	runtimeVersion string,
	command string,
	capabilities []string,
	options ...Option,
) (*Emitter, error) {
	// 入口立即复制，避免调用方在校验后通过共享底层数组改变 hello。
	capabilitySnapshot := append([]string(nil), capabilities...)
	settings := emitterOptions{clock: time.Now}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("protocol option must not be nil")
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}

	// hello 是 capabilities 的唯一构造点，冻结全集在这里收口。
	for _, advertised := range capabilitySnapshot {
		if !IsKnownCapability(Capability(advertised)) {
			return nil, fmt.Errorf("unknown protocol capability %q", advertised)
		}
	}

	if settings.operationID == "" {
		operationID, err := newRandomOperationID(settings.clock())
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

	if err := emitter.emit(TypeHello, &HelloEvent{
		RuntimeVersion: runtimeVersion,
		Command:        command,
		Capabilities:   capabilitySnapshot,
	}); err != nil {
		return nil, err
	}
	return emitter, nil
}

// OperationID 返回此 Emitter 所有事件共享的 ULID。
func (e *Emitter) OperationID() string {
	return e.operationID
}

// EmitProgress 发射 progress 事件。
func (e *Emitter) EmitProgress(event ProgressEvent) error {
	return e.emit(TypeProgress, &event)
}

// EmitState 发射生命周期 state 事件。
func (e *Emitter) EmitState(event StateEvent) error {
	return e.emit(TypeState, &event)
}

// EmitLog 发射一行受管进程日志。
func (e *Emitter) EmitLog(event LogEvent) error {
	return e.emit(TypeLog, &event)
}

// EmitWarning 发射非终态 warning。
func (e *Emitter) EmitWarning(event WarningEvent) error {
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if err := e.output.rejectLocked(TypeWarning); err != nil {
		return err
	}
	// 必须先归一化再快照：否则账本里留 nil、发出的事件却是空容器，
	// result.details.warnings[i] 会与原 warning 事件不再逐字段相等。
	event.normalize()
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

// EmitError 发射操作 error。
func (e *Emitter) EmitError(event ErrorEvent) error {
	return e.emit(TypeError, &event)
}

// EmitResult 发射操作 result。
func (e *Emitter) EmitResult(event ResultEvent) error {
	e.output.mu.Lock()
	defer e.output.mu.Unlock()

	if err := e.output.rejectLocked(TypeResult); err != nil {
		return err
	}
	event.Details = e.output.resultDetails(event.Details)
	return e.emitPreparedLocked(TypeResult, &event)
}

func (e *Emitter) emit(eventType EventType, event protocolEvent) error {
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

func (e *Emitter) emitPreparedLocked(eventType EventType, event protocolEvent) error {
	event.normalize()
	event.setCommon(Common{
		Protocol:    Version,
		Type:        eventType,
		OperationID: e.operationID,
		Sequence:    e.output.nextSequence,
		Timestamp:   e.clock().Format(time.RFC3339Nano),
	})
	terminalReserved := eventType == TypeResult
	if terminalReserved {
		// 在可能阻塞的 renderer 前预约终态，使唯一 result 与解锁顺序成为显式不变量。
		// renderer 失败时仍在同一锁域回滚，保留非粘性编码错误的重试语义。
		e.output.terminal = true
	}
	if err := renderEvent(e.output.renderer, event); err != nil {
		if terminalReserved {
			e.output.terminal = false
		}
		var nonSticky *nonStickyRenderError
		if errors.As(err, &nonSticky) {
			return nonSticky.err
		}
		return e.output.rememberWriteError(err)
	}

	e.output.nextSequence++
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

// resultDetails 构造 result 的 details。它总是返回新的顶层 map，
// 既不修改调用方的 map，也不会让 details 变成 null。
func (o *ProcessOutput) resultDetails(details map[string]any) map[string]any {
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
	o.writeErr = &outputWriteError{cause: err}
	return o.writeErr
}

// protocolEvent 是可发射的协议事件：既能被写入公共字段，也能把空容器字段
// 归一化。归一化做成接口方法而不是各 Emit 方法里的散装判断，是为了让新增
// 事件类型在编译期就必须表态，同时覆盖不经公开 Emit 方法的 hello 路径。
type protocolEvent interface {
	setCommon(Common)
	normalize()
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

func (e *HelloEvent) normalize() {
	if e.Capabilities == nil {
		e.Capabilities = []string{}
	}
}

// normalize 对 progress 是空操作：该事件没有容器字段。
func (e *ProgressEvent) normalize() {}

func (e *StateEvent) normalize() {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
}

// normalize 对 log 是空操作：该事件没有容器字段。
func (e *LogEvent) normalize() {}

func (e *WarningEvent) normalize() {
	e.Remediation, e.Details = normalizeContainers(e.Remediation, e.Details)
}

func (e *ErrorEvent) normalize() {
	e.Remediation, e.Details = normalizeContainers(e.Remediation, e.Details)
}

func (e *ResultEvent) normalize() {
	e.Remediation, e.Details = normalizeContainers(e.Remediation, e.Details)
}

// normalizeContainers 把 nil 的 remediation 与 details 换成空容器，
// 保证 wire 上这两个字段永远是数组和对象，不出现 null。
func normalizeContainers(remediation []string, details map[string]any) ([]string, map[string]any) {
	if remediation == nil {
		remediation = []string{}
	}
	if details == nil {
		details = map[string]any{}
	}
	return remediation, details
}
