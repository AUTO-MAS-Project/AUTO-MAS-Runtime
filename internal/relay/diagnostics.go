package relay

import (
	"errors"
	"syscall"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// diagnoseAttempt 只写当前尝试的安全标量，不保存错误、地址或响应正文。
func diagnoseAttempt(diagnostics []*AttemptOutcome, kind, operation string, status int, cause error) {
	if len(diagnostics) == 0 || diagnostics[0] == nil {
		return
	}
	d := diagnostics[0]
	d.FailureKind, d.Operation = kind, operation
	if status >= 100 && status <= 599 {
		d.HTTPStatus = status
	}
	var errno syscall.Errno
	if errors.As(cause, &errno) {
		d.WindowsError = uint64(errno)
	}
}

func diagnoseNetwork(diagnostics []*AttemptOutcome, cause error) {
	diagnoseAttempt(diagnostics, mirror.NetworkFailureKind(cause).String(), "request", 0, cause)
}

func (e *engine) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

func (e *engine) finishAttempt(attempt *AttemptOutcome, outcome Outcome, startedAt time.Time) {
	attempt.Outcome = outcome
	attempt.Duration = max(time.Duration(0), e.now().Sub(startedAt))
	if attempt.FailureKind == "" {
		attempt.FailureKind = outcome.String()
	}
}

// recordLocalFailure 保留尚未归属上游的文件级故障，不改变失败源或回退选择。
func (e *engine) recordLocalFailure(plan *chunkPlan, kind, operation string, cause error) {
	attempt := AttemptOutcome{Outcome: OutcomeNetwork}
	diagnoseAttempt([]*AttemptOutcome{&attempt}, kind, operation, 0, cause)
	plan.mu.Lock()
	plan.attempts = append(plan.attempts, attempt)
	plan.mu.Unlock()
}

func attemptLogFields(attempt AttemptOutcome, route Route, item string) map[string]any {
	fields := map[string]any{
		"route": route.String(), "item": item, "source": attempt.Source, "outcome": attempt.Outcome,
		"failureKind": attempt.FailureKind, "durationMs": max(int64(0), attempt.Duration.Milliseconds()),
	}
	if attempt.Operation != "" {
		fields["operation"] = attempt.Operation
	}
	if attempt.HTTPStatus != 0 {
		fields["httpStatus"] = attempt.HTTPStatus
	}
	if attempt.WindowsError != 0 {
		fields["windowsError"] = attempt.WindowsError
	}
	return fields
}
