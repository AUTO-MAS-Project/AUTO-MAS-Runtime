package mirror

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	defaultMaxSourceAttempts = 2
	defaultRetryDelay        = 250 * time.Millisecond
	reportTimeout            = 5 * time.Second
	maxSourceAttemptsLimit   = 16
)

var (
	ErrInvalidRotationOption = errors.New(
		"mirror rotation option is invalid",
	)
	ErrInvalidRotationRequest = errors.New(
		"mirror rotation request is invalid",
	)
)

// RotationOption 配置构造后只读的 Rotator。
type RotationOption func(*rotationOptions) error

// WaitFunc 在同一 Source 重试前执行可取消等待。
type WaitFunc func(ctx context.Context, delay time.Duration) error

// reportContextFactory 为每份报告创建独立收口 context。
type reportContextFactory func(
	operationCtx context.Context,
) (context.Context, context.CancelFunc)

type rotationOptions struct {
	clock             func() time.Time
	wait              WaitFunc
	reporter          Reporter
	maxSourceAttempts int
	retryDelay        time.Duration

	clockSet      bool
	waitSet       bool
	reporterSet   bool
	attemptsSet   bool
	retryDelaySet bool
}

// Rotator 保存不可变的轮换依赖与有限重试参数。
// 同一 Rotator 可被多个 goroutine 并发调用 Run。
type Rotator struct {
	clock             func() time.Time
	wait              WaitFunc
	reporter          Reporter
	reportContext     reportContextFactory
	maxSourceAttempts int
	retryDelay        time.Duration
}

// NewRotator 使用生产报告 context factory 构造只读 Rotator。
func NewRotator(options ...RotationOption) (*Rotator, error) {
	return newRotatorWithDependencies(newReportContext, options...)
}

// WithRotatorClock 注入报告计时使用的时钟。
func WithRotatorClock(clock func() time.Time) RotationOption {
	return func(options *rotationOptions) error {
		if options == nil || clock == nil || options.clockSet {
			return fmt.Errorf("%w: clock", ErrInvalidRotationOption)
		}
		options.clock = clock
		options.clockSet = true
		return nil
	}
}

// WithRotatorWait 注入同源重试等待。
func WithRotatorWait(wait WaitFunc) RotationOption {
	return func(options *rotationOptions) error {
		if options == nil || wait == nil || options.waitSet {
			return fmt.Errorf("%w: wait", ErrInvalidRotationOption)
		}
		options.wait = wait
		options.waitSet = true
		return nil
	}
}

// WithAttemptReporter 注入同步 AttemptReport 接收方。
func WithAttemptReporter(reporter Reporter) RotationOption {
	return func(options *rotationOptions) error {
		if options == nil ||
			nilReporter(reporter) ||
			options.reporterSet {
			return fmt.Errorf("%w: reporter", ErrInvalidRotationOption)
		}
		options.reporter = reporter
		options.reporterSet = true
		return nil
	}
}

// WithMaxSourceAttempts 配置每个 Source 的有限总尝试次数。
func WithMaxSourceAttempts(attempts int) RotationOption {
	return func(options *rotationOptions) error {
		if options == nil ||
			attempts < 1 ||
			attempts > maxSourceAttemptsLimit ||
			options.attemptsSet {
			return fmt.Errorf(
				"%w: max source attempts",
				ErrInvalidRotationOption,
			)
		}
		options.maxSourceAttempts = attempts
		options.attemptsSet = true
		return nil
	}
}

// WithRetryDelay 配置同源重试前的正等待时长。
func WithRetryDelay(delay time.Duration) RotationOption {
	return func(options *rotationOptions) error {
		if options == nil || delay <= 0 || options.retryDelaySet {
			return fmt.Errorf("%w: retry delay", ErrInvalidRotationOption)
		}
		options.retryDelay = delay
		options.retryDelaySet = true
		return nil
	}
}

func newRotatorWithDependencies(
	factory reportContextFactory,
	options ...RotationOption,
) (*Rotator, error) {
	if factory == nil {
		return nil, fmt.Errorf(
			"%w: report context factory",
			ErrInvalidRotationOption,
		)
	}
	configured := rotationOptions{
		clock:             time.Now,
		wait:              defaultWait,
		maxSourceAttempts: defaultMaxSourceAttempts,
		retryDelay:        defaultRetryDelay,
	}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf(
				"%w: nil option at index %d",
				ErrInvalidRotationOption,
				index,
			)
		}
		if err := option(&configured); err != nil {
			return nil, newSafeError(
				safeErrorOption,
				ErrInvalidRotationOption,
				err,
			)
		}
	}
	if !validRotationOptions(configured) {
		return nil, fmt.Errorf(
			"%w: final values",
			ErrInvalidRotationOption,
		)
	}
	return &Rotator{
		clock:             configured.clock,
		wait:              configured.wait,
		reporter:          configured.reporter,
		reportContext:     factory,
		maxSourceAttempts: configured.maxSourceAttempts,
		retryDelay:        configured.retryDelay,
	}, nil
}

func validRotationOptions(options rotationOptions) bool {
	return options.clock != nil &&
		options.wait != nil &&
		(options.reporter == nil || !nilReporter(options.reporter)) &&
		options.maxSourceAttempts >= 1 &&
		options.maxSourceAttempts <= maxSourceAttemptsLimit &&
		options.retryDelay > 0
}

func nilReporter(reporter Reporter) bool {
	if reporter == nil {
		return true
	}
	value := reflect.ValueOf(reporter)
	switch value.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func newReportContext(
	operationCtx context.Context,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		context.WithoutCancel(operationCtx),
		reportTimeout,
	)
}

func defaultWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run 按 Plan 顺序执行有限尝试并保持 Target 不变。
func (r *Rotator) Run(
	ctx context.Context,
	plan Plan,
	target Target,
	attempt AttemptFunc,
) (RotationResult, error) {
	if err := validateRotationRequest(
		r,
		ctx,
		plan,
		target,
		attempt,
	); err != nil {
		return RotationResult{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RotationResult{}, newSafeError(
			safeErrorCancellation,
			ctxErr,
		)
	}
	if plan.offline {
		return RotationResult{}, newRotationError(
			protocol.CodeNetworkUnavailable,
			nil,
			nil,
		)
	}

	reports := make(
		[]AttemptReport,
		0,
		len(plan.sources)*r.maxSourceAttempts,
	)
	causes := make(
		[]error,
		0,
		len(plan.sources)*r.maxSourceAttempts,
	)
	blacklisted := make(map[string]struct{}, len(plan.sources))
	hadIntegrityFailure := false
	globalTry := 0

	for _, source := range plan.sources {
		if _, blocked := blacklisted[source.key]; blocked {
			continue
		}
		for sourceTry := 1; sourceTry <= r.maxSourceAttempts; sourceTry++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failedRotationResult(reports), newSafeError(
					safeErrorCancellation,
					ctxErr,
				)
			}
			if validateTarget(target) != nil ||
				target.Fingerprint() == "" {
				targetErr := fmt.Errorf(
					"%w: target changed during rotation",
					ErrInvalidRotationRequest,
				)
				return failedRotationResult(reports), newSafeError(
					safeErrorTarget,
					targetErr,
				)
			}

			globalTry++
			currentAttempt := Attempt{
				Source:    source,
				Target:    target,
				SourceTry: sourceTry,
				GlobalTry: globalTry,
			}
			startedAt := r.clock()
			outcome := attempt(ctx, currentAttempt)
			finishedAt := r.clock()
			outcomeErr := validateAttemptOutcome(plan.kind, outcome)
			callbackErr := wrapSafeError(
				safeErrorAttempt,
				outcome.Err,
			)
			legalSuccess := outcomeErr == nil &&
				outcome.Kind == OutcomeSucceeded

			reportOutcome := outcome
			if outcomeErr != nil {
				reportOutcome = AttemptOutcome{Err: outcome.Err}
			}
			var ctxErrBeforeReport error
			if !legalSuccess {
				ctxErrBeforeReport = ctx.Err()
				if ctxErrBeforeReport != nil {
					reportOutcome = AttemptOutcome{
						Kind: OutcomeCancelled,
						Err:  ctxErrBeforeReport,
					}
				}
			}
			report := buildAttemptReport(
				currentAttempt,
				startedAt,
				finishedAt,
				reportOutcome,
			)
			reports = append(reports, report)
			reporterErr := r.reportAttempt(ctx, report)

			if legalSuccess {
				result := RotationResult{
					Source:       source,
					ActualCommit: outcome.ActualCommit,
					Reports:      cloneAttemptReports(reports),
				}
				if reporterErr != nil {
					return result, reporterErr
				}
				return result, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failedRotationResult(reports), newSafeError(
					safeErrorCancellation,
					errors.Join(
						ctxErr,
						outcomeErr,
						callbackErr,
						reporterErr,
					),
				)
			}
			if outcomeErr != nil {
				return failedRotationResult(reports), newSafeError(
					safeErrorAttemptContract,
					errors.Join(
						outcomeErr,
						callbackErr,
						reporterErr,
					),
				)
			}
			if reporterErr != nil {
				return failedRotationResult(reports), newSafeError(
					safeErrorReporter,
					errors.Join(
						errors.Join(causes...),
						callbackErr,
						reporterErr,
					),
				)
			}

			causes = append(causes, callbackErr)
			switch outcome.Kind {
			case OutcomeRetrySameSource:
				if sourceTry == r.maxSourceAttempts {
					break
				}
				waitErr := r.wait(ctx, r.retryDelay)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return failedRotationResult(reports), newSafeError(
						safeErrorCancellation,
						errors.Join(
							ctxErr,
							errors.Join(causes...),
							wrapSafeError(safeErrorWait, waitErr),
						),
					)
				}
				if waitErr != nil {
					return failedRotationResult(reports), newSafeError(
						safeErrorWait,
						errors.Join(
							errors.Join(causes...),
							wrapSafeError(safeErrorWait, waitErr),
						),
					)
				}
				continue
			case OutcomeSwitchSource:
				sourceTry = r.maxSourceAttempts
			case OutcomeIntegrityFailure:
				hadIntegrityFailure = true
				blacklisted[source.key] = struct{}{}
				sourceTry = r.maxSourceAttempts
			case OutcomeTargetFailure:
				return failedRotationResult(reports), newSafeError(
					safeErrorTarget,
					callbackErr,
				)
			}
		}
	}

	result := failedRotationResult(reports)
	if hadIntegrityFailure {
		return result, newIntegrityExhaustedError(reports, causes)
	}
	return result, newRotationError(
		protocol.CodeMirrorExhausted,
		reports,
		causes,
	)
}

func validateRotationRequest(
	rotator *Rotator,
	ctx context.Context,
	plan Plan,
	target Target,
	attempt AttemptFunc,
) error {
	if !validRotator(rotator) ||
		ctx == nil ||
		attempt == nil ||
		!validPlan(plan) ||
		target.ValidateForKind(plan.kind) != nil {
		return fmt.Errorf(
			"%w: request invariant",
			ErrInvalidRotationRequest,
		)
	}
	return nil
}

func validRotator(rotator *Rotator) bool {
	return rotator != nil &&
		rotator.clock != nil &&
		rotator.wait != nil &&
		rotator.reportContext != nil &&
		(rotator.reporter == nil || !nilReporter(rotator.reporter)) &&
		rotator.maxSourceAttempts >= 1 &&
		rotator.maxSourceAttempts <= maxSourceAttemptsLimit &&
		rotator.retryDelay > 0
}

func validateAttemptOutcome(kind Kind, outcome AttemptOutcome) error {
	if !outcome.Kind.Valid() || outcome.Kind == OutcomeCancelled {
		return fmt.Errorf(
			"%w: outcome kind",
			ErrInvalidRotationRequest,
		)
	}
	if outcome.Kind == OutcomeSucceeded {
		if outcome.Err != nil ||
			outcome.FailureKind != "" ||
			(kind == KindGit && !validGitCommit(outcome.ActualCommit)) ||
			(kind != KindGit && outcome.ActualCommit != "") {
			return fmt.Errorf(
				"%w: success outcome",
				ErrInvalidRotationRequest,
			)
		}
		return nil
	}
	if outcome.Err == nil ||
		outcome.ActualCommit != "" {
		return fmt.Errorf(
			"%w: failure outcome",
			ErrInvalidRotationRequest,
		)
	}
	if !outcome.FailureKind.Valid() {
		return fmt.Errorf(
			"%w: failure diagnostic",
			ErrInvalidRotationRequest,
		)
	}
	return nil
}

func validGitCommit(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for i := 0; i < len(commit); i++ {
		character := commit[i]
		if (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func buildAttemptReport(
	attempt Attempt,
	startedAt time.Time,
	finishedAt time.Time,
	outcome AttemptOutcome,
) AttemptReport {
	duration := finishedAt.Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	return AttemptReport{
		Kind:         attempt.Source.kind,
		SourceKey:    attempt.Source.key,
		SourceTry:    attempt.SourceTry,
		GlobalTry:    attempt.GlobalTry,
		Target:       attempt.Target,
		TargetHash:   attempt.Target.Fingerprint(),
		StartedAt:    startedAt,
		Duration:     duration,
		Outcome:      outcome.Kind,
		FailureKind:  outcome.FailureKind,
		Error:        attemptErrorText(outcome),
		ActualCommit: outcome.ActualCommit,
	}
}

func failedRotationResult(reports []AttemptReport) RotationResult {
	return RotationResult{Reports: cloneAttemptReports(reports)}
}

// reportAttempt 使用独立收口 context 同步报告一次实际 Attempt。
func (r *Rotator) reportAttempt(
	operationCtx context.Context,
	report AttemptReport,
) error {
	if r.reporter == nil {
		return nil
	}
	reportCtx, cancel := r.reportContext(operationCtx)
	if reportCtx == nil || cancel == nil {
		if cancel != nil {
			cancel()
		}
		return newSafeError(
			safeErrorAttemptContract,
			ErrInvalidRotationRequest,
		)
	}
	defer cancel()
	if err := r.reporter.ReportAttempt(reportCtx, report); err != nil {
		return newSafeError(safeErrorReporter, err)
	}
	return nil
}
