package mirror

import (
	"context"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// OutcomeKind 是 AttemptFunc 返回的结构化状态转换。
type OutcomeKind string

const (
	OutcomeSucceeded        OutcomeKind = "succeeded"
	OutcomeRetrySameSource  OutcomeKind = "retry_same_source"
	OutcomeSwitchSource     OutcomeKind = "switch_source"
	OutcomeIntegrityFailure OutcomeKind = "integrity_failure"
	OutcomeTargetFailure    OutcomeKind = "target_failure"
	OutcomeCancelled        OutcomeKind = "cancelled"
)

// String 返回 OutcomeKind 的稳定字面量。
func (k OutcomeKind) String() string {
	return string(k)
}

// Valid 报告 OutcomeKind 是否属于冻结全集。
func (k OutcomeKind) Valid() bool {
	switch k {
	case OutcomeSucceeded,
		OutcomeRetrySameSource,
		OutcomeSwitchSource,
		OutcomeIntegrityFailure,
		OutcomeTargetFailure,
		OutcomeCancelled:
		return true
	default:
		return false
	}
}

// FailureKind 是不参与状态迁移的开放诊断标识。
type FailureKind string

// String 返回 FailureKind 的稳定字面量。
func (k FailureKind) String() string {
	return string(k)
}

// Valid 报告 FailureKind 是否满足开放的安全 snake_case 语法。
func (k FailureKind) Valid() bool {
	if len(k) == 0 || len(k) > 64 {
		return false
	}
	previousUnderscore := false
	for i := 0; i < len(k); i++ {
		character := k[i]
		switch {
		case character >= 'a' && character <= 'z':
			previousUnderscore = false
		case character >= '0' && character <= '9':
			if i == 0 {
				return false
			}
			previousUnderscore = false
		case character == '_':
			if i == 0 || i == len(k)-1 || previousUnderscore {
				return false
			}
			previousUnderscore = true
		default:
			return false
		}
	}
	return true
}

// Attempt 描述一次具体 Source 尝试的不可变输入。
type Attempt struct {
	Source    Source
	Target    Target
	SourceTry int
	GlobalTry int
}

// AttemptOutcome 是 AttemptFunc 返回的结构化结果。
type AttemptOutcome struct {
	Kind         OutcomeKind
	FailureKind  FailureKind
	ActualCommit string
	Err          error
}

// AttemptFunc 对一个 Source 与原始 Target 执行一次调用方操作。
type AttemptFunc func(
	ctx context.Context,
	attempt Attempt,
) AttemptOutcome

// RotationResult 保存成功源、Git 实际 Commit 和有序报告。
type RotationResult struct {
	Source       Source
	ActualCommit string
	Reports      []AttemptReport
}

// AttemptReport 保存一次实际 Attempt 的稳定诊断字段。
type AttemptReport struct {
	Kind         Kind
	SourceKey    string
	SourceTry    int
	GlobalTry    int
	Target       Target
	TargetHash   string
	StartedAt    time.Time
	Duration     time.Duration
	Outcome      OutcomeKind
	FailureKind  FailureKind
	Error        string
	ActualCommit string
}

// Reporter 同步持久化一次已冻结的 AttemptReport。
// 有状态实现会被同一 Rotator 的并发 Run 调用，必须自行保证并发安全。
type Reporter interface {
	ReportAttempt(ctx context.Context, report AttemptReport) error
}

type safeErrorKind uint8

const (
	safeErrorCancellation safeErrorKind = iota + 1
	safeErrorAttemptContract
	safeErrorReporter
	safeErrorWait
	safeErrorTarget
	safeErrorAttempt
	safeErrorOption
)

type safeError struct {
	kind   safeErrorKind
	causes []error
}

func (e *safeError) Error() string {
	if e == nil {
		return "mirror operation failed"
	}
	switch e.kind {
	case safeErrorCancellation:
		return "mirror operation cancelled"
	case safeErrorAttemptContract:
		return "mirror attempt contract is invalid"
	case safeErrorReporter:
		return "mirror attempt reporting failed"
	case safeErrorWait:
		return "mirror retry wait failed"
	case safeErrorTarget:
		return "mirror target failed"
	case safeErrorAttempt:
		return "mirror attempt failed"
	case safeErrorOption:
		return "mirror rotation option is invalid"
	default:
		return "mirror operation failed"
	}
}

func (e *safeError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return append([]error(nil), e.causes...)
}

func newSafeError(kind safeErrorKind, causes ...error) error {
	return &safeError{
		kind:   kind,
		causes: compactCauses(causes),
	}
}

func wrapSafeError(kind safeErrorKind, cause error) error {
	if cause == nil {
		return nil
	}
	return newSafeError(kind, cause)
}

// RotationError 表示离线或全部普通 Source 自然耗尽。
type RotationError struct {
	code    protocol.Code
	reports []AttemptReport
	causes  []error
}

// Error 返回不包含 Source URL 或底层错误文本的稳定诊断。
func (e *RotationError) Error() string {
	if e == nil {
		return "mirror rotation error"
	}
	switch e.code {
	case protocol.CodeNetworkUnavailable:
		return "network is unavailable"
	case protocol.CodeMirrorExhausted:
		return "mirror sources exhausted"
	default:
		return "mirror rotation failed"
	}
}

// Code 返回供上层映射的稳定协议错误码。
func (e *RotationError) Code() protocol.Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Reports 返回有序 AttemptReport 的防御性副本。
func (e *RotationError) Reports() []AttemptReport {
	if e == nil {
		return nil
	}
	return cloneAttemptReports(e.reports)
}

// Unwrap 返回底层错误链的防御性副本。
func (e *RotationError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return append([]error(nil), e.causes...)
}

// IntegrityExhaustedError 表示自然耗尽中至少发生一次完整性失败。
type IntegrityExhaustedError struct {
	reports []AttemptReport
	causes  []error
}

// Error 返回不包含 Source URL 或底层错误文本的稳定诊断。
func (e *IntegrityExhaustedError) Error() string {
	return "mirror integrity checks exhausted"
}

// Reports 返回有序 AttemptReport 的防御性副本。
func (e *IntegrityExhaustedError) Reports() []AttemptReport {
	if e == nil {
		return nil
	}
	return cloneAttemptReports(e.reports)
}

// Unwrap 返回底层错误链的防御性副本。
func (e *IntegrityExhaustedError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return append([]error(nil), e.causes...)
}

func newRotationError(
	code protocol.Code,
	reports []AttemptReport,
	causes []error,
) *RotationError {
	return &RotationError{
		code:    code,
		reports: cloneAttemptReports(reports),
		causes:  wrapSafeCauses(safeErrorAttempt, causes),
	}
}

func newIntegrityExhaustedError(
	reports []AttemptReport,
	causes []error,
) *IntegrityExhaustedError {
	return &IntegrityExhaustedError{
		reports: cloneAttemptReports(reports),
		causes:  wrapSafeCauses(safeErrorAttempt, causes),
	}
}

func cloneAttemptReports(reports []AttemptReport) []AttemptReport {
	return append([]AttemptReport(nil), reports...)
}

func compactCauses(causes []error) []error {
	compacted := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			compacted = append(compacted, cause)
		}
	}
	return compacted
}

func wrapSafeCauses(
	kind safeErrorKind,
	causes []error,
) []error {
	wrapped := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			wrapped = append(wrapped, wrapSafeError(kind, cause))
		}
	}
	return wrapped
}

func attemptErrorText(outcome AttemptOutcome) string {
	if outcome.Err == nil {
		return ""
	}
	if outcome.FailureKind.Valid() {
		return "attempt failed: " + outcome.FailureKind.String()
	}
	return "attempt failed"
}

var (
	_ error                         = (*RotationError)(nil)
	_ error                         = (*IntegrityExhaustedError)(nil)
	_ error                         = (*safeError)(nil)
	_ interface{ Unwrap() []error } = (*RotationError)(nil)
	_ interface{ Unwrap() []error } = (*IntegrityExhaustedError)(nil)
	_ interface{ Unwrap() []error } = (*safeError)(nil)
)
