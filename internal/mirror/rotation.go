package mirror

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
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
