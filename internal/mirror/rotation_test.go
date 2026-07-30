package mirror

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestOutcomeKindAndFailureKind_ValidateStableSyntax(t *testing.T) {
	outcomes := []OutcomeKind{
		OutcomeSucceeded,
		OutcomeRetrySameSource,
		OutcomeSwitchSource,
		OutcomeIntegrityFailure,
		OutcomeTargetFailure,
		OutcomeCancelled,
	}
	for _, outcome := range outcomes {
		if !outcome.Valid() || outcome.String() != string(outcome) {
			t.Errorf(
				"OutcomeKind(%q) = valid %t/string %q",
				outcome,
				outcome.Valid(),
				outcome.String(),
			)
		}
	}
	for _, outcome := range []OutcomeKind{"", "success", "cancelled_by_callback"} {
		if outcome.Valid() {
			t.Errorf("OutcomeKind(%q).Valid() = true, want false", outcome)
		}
	}

	pattern, err := regexp.Compile(
		`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`,
	)
	if err != nil {
		t.Fatalf("regexp.Compile() error = %v", err)
	}
	validFailures := []FailureKind{
		"a",
		"network_temporary",
		"connect_timeout",
		"checksum_mismatch",
		"future_transport_reason",
		FailureKind("a" + strings.Repeat("1", 63)),
	}
	for _, failure := range validFailures {
		if !failure.Valid() ||
			!pattern.MatchString(failure.String()) ||
			failure.String() != string(failure) {
			t.Errorf(
				"FailureKind(%q) = valid %t/string %q",
				failure,
				failure.Valid(),
				failure.String(),
			)
		}
	}
	for _, failure := range []FailureKind{
		"",
		"DNS",
		"1timeout",
		"_timeout",
		"timeout_",
		"double__separator",
		"has-hyphen",
		"含中文",
		FailureKind("a" + strings.Repeat("1", 64)),
	} {
		if failure.Valid() {
			t.Errorf("FailureKind(%q).Valid() = true, want false", failure)
		}
		if len(failure) <= 64 &&
			pattern.MatchString(failure.String()) {
			t.Errorf(
				"FailureKind(%q) unexpectedly matches syntax",
				failure,
			)
		}
	}
}

func TestRotationErrors_DefensiveReportsCausesAndCodes(t *testing.T) {
	causeOne := errors.New(
		"GET https://user:password@example.invalid/archive?token=secret",
	)
	causeTwo := errors.New("connection reset")
	inputReports := []AttemptReport{{SourceKey: "cnb", GlobalTry: 1}}
	inputCauses := []error{causeOne, nil, causeTwo}

	rotationErr := newRotationError(
		protocol.CodeMirrorExhausted,
		inputReports,
		inputCauses,
	)
	inputReports[0].SourceKey = "changed"
	inputCauses[0] = errors.New("changed")

	if got := rotationErr.Code(); got != protocol.CodeMirrorExhausted {
		t.Fatalf("Code() = %q, want %q", got, protocol.CodeMirrorExhausted)
	}
	if got := rotationErr.Reports(); len(got) != 1 ||
		got[0].SourceKey != "cnb" {
		t.Fatalf("Reports() = %#v, want original report", got)
	}
	firstReports := rotationErr.Reports()
	firstReports[0].SourceKey = "mutated"
	if got := rotationErr.Reports()[0].SourceKey; got != "cnb" {
		t.Fatalf("Reports() exposed storage: %q", got)
	}
	firstCauses := rotationErr.Unwrap()
	if len(firstCauses) != 2 ||
		!errors.Is(firstCauses[0], causeOne) ||
		!errors.Is(firstCauses[1], causeTwo) ||
		!errors.Is(rotationErr, causeOne) ||
		!errors.Is(rotationErr, causeTwo) {
		t.Fatalf(
			"Unwrap() = %#v, want ordered preserved causes",
			firstCauses,
		)
	}
	for _, cause := range firstCauses {
		if strings.Contains(cause.Error(), "example.invalid") ||
			strings.Contains(cause.Error(), "connection reset") {
			t.Fatalf("Unwrap() exposed raw cause text: %q", cause)
		}
	}
	firstCauses[0] = errors.New("mutated")
	if got := rotationErr.Unwrap()[0]; !errors.Is(got, causeOne) {
		t.Fatalf("Unwrap() exposed storage: %v", got)
	}
	for _, forbidden := range []string{
		"user:password",
		"token=secret",
		"example.invalid",
	} {
		if strings.Contains(rotationErr.Error(), forbidden) {
			t.Fatalf("Error() = %q, contains %q", rotationErr.Error(), forbidden)
		}
	}

	integrityErr := newIntegrityExhaustedError(inputReports, []error{
		causeOne,
		causeTwo,
	})
	if len(integrityErr.Reports()) != 1 ||
		len(integrityErr.Unwrap()) != 2 ||
		!errors.Is(integrityErr.Unwrap()[0], causeOne) ||
		!errors.Is(integrityErr.Unwrap()[1], causeTwo) ||
		!errors.Is(integrityErr, causeOne) {
		t.Fatalf("IntegrityExhaustedError lost reports or causes")
	}
	for _, cause := range integrityErr.Unwrap() {
		if strings.Contains(cause.Error(), "example.invalid") ||
			strings.Contains(cause.Error(), "connection reset") {
			t.Fatalf("Integrity Unwrap() exposed raw cause text: %q", cause)
		}
	}
	integrityReports := integrityErr.Reports()
	integrityReports[0].SourceKey = "mutated"
	if got := integrityErr.Reports()[0].SourceKey; got != "changed" {
		t.Fatalf("Integrity Reports() exposed storage: %q", got)
	}
	if strings.Contains(integrityErr.Error(), "example.invalid") {
		t.Fatalf("Integrity Error() leaked cause: %q", integrityErr.Error())
	}
}

func TestSafeError_UsesStableTextAndPreservesCauses(t *testing.T) {
	secretCause := &safeCauseError{
		text: "GET https://user:password@example.invalid/file?token=secret#fragment",
	}
	secondCause := errors.New("secondary cause")
	tests := []struct {
		name string
		kind safeErrorKind
		want string
	}{
		{
			name: "cancellation",
			kind: safeErrorCancellation,
			want: "mirror operation cancelled",
		},
		{
			name: "attempt contract",
			kind: safeErrorAttemptContract,
			want: "mirror attempt contract is invalid",
		},
		{
			name: "reporter",
			kind: safeErrorReporter,
			want: "mirror attempt reporting failed",
		},
		{
			name: "wait",
			kind: safeErrorWait,
			want: "mirror retry wait failed",
		},
		{
			name: "target",
			kind: safeErrorTarget,
			want: "mirror target failed",
		},
		{
			name: "attempt",
			kind: safeErrorAttempt,
			want: "mirror attempt failed",
		},
		{
			name: "option",
			kind: safeErrorOption,
			want: "mirror rotation option is invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newSafeError(
				test.kind,
				secretCause,
				nil,
				secondCause,
			)
			if err.Error() != test.want {
				t.Fatalf("Error() = %q, want %q", err.Error(), test.want)
			}
			if !errors.Is(err, secretCause) ||
				!errors.Is(err, secondCause) {
				t.Fatalf("safe error lost causes: %v", err)
			}
			var typedCause *safeCauseError
			if !errors.As(err, &typedCause) ||
				typedCause != secretCause {
				t.Fatalf(
					"errors.As(*safeCauseError) = %#v, want original",
					typedCause,
				)
			}
			for _, forbidden := range []string{
				"user:password",
				"example.invalid",
				"token=secret",
				"fragment",
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf(
						"Error() = %q, contains %q",
						err,
						forbidden,
					)
				}
			}
			var safeErr *safeError
			if !errors.As(err, &safeErr) {
				t.Fatalf("errors.As(*safeError) = false for %T", err)
			}
			causes := safeErr.Unwrap()
			if len(causes) != 2 ||
				!errors.Is(causes[0], secretCause) ||
				!errors.Is(causes[1], secondCause) {
				t.Fatalf("Unwrap() = %#v, want ordered causes", causes)
			}
			causes[0] = errors.New("mutated")
			if !errors.Is(safeErr.Unwrap()[0], secretCause) {
				t.Fatal("safeError.Unwrap() exposed cause slice")
			}
		})
	}

	joined := errors.Join(
		newSafeError(safeErrorAttempt, secretCause),
		newSafeError(safeErrorReporter, secondCause),
		context.Canceled,
	)
	terminal := newSafeError(safeErrorCancellation, joined)
	if terminal.Error() != "mirror operation cancelled" ||
		!errors.Is(terminal, secretCause) ||
		!errors.Is(terminal, secondCause) ||
		!errors.Is(terminal, context.Canceled) {
		t.Fatalf("joined safe terminal = %v, lost stable text or causes", terminal)
	}
	if strings.Contains(terminal.Error(), "example.invalid") {
		t.Fatalf("joined safe terminal leaked cause: %v", terminal)
	}
}

func TestNewRotator_DefaultsAndInjectedDependencies(t *testing.T) {
	rotator, err := NewRotator()
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	if rotator.maxSourceAttempts != 2 {
		t.Fatalf(
			"maxSourceAttempts = %d, want 2",
			rotator.maxSourceAttempts,
		)
	}
	if rotator.retryDelay != 250*time.Millisecond {
		t.Fatalf("retryDelay = %v, want 250ms", rotator.retryDelay)
	}
	if rotator.clock == nil ||
		rotator.wait == nil ||
		rotator.reportContext == nil {
		t.Fatal("NewRotator() left a required dependency nil")
	}
	if rotator.reporter != nil {
		t.Fatalf("default reporter = %#v, want nil", rotator.reporter)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rotator.wait(cancelledCtx, time.Hour); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("default wait error = %v, want context.Canceled", err)
	}

	type contextKey struct{}
	operationCtx, cancelOperation := context.WithCancel(context.WithValue(
		context.Background(),
		contextKey{},
		"operation-value",
	))
	cancelOperation()
	reportCtx, reportCancel := rotator.reportContext(operationCtx)
	if reportCtx.Err() != nil {
		reportCancel()
		t.Fatalf(
			"report context inherited operation cancellation: %v",
			reportCtx.Err(),
		)
	}
	if got := reportCtx.Value(contextKey{}); got != "operation-value" {
		reportCancel()
		t.Fatalf("report context value = %#v, want operation-value", got)
	}
	deadline, ok := reportCtx.Deadline()
	if !ok ||
		!deadline.After(time.Now()) ||
		deadline.Sub(time.Now()) > reportTimeout {
		reportCancel()
		t.Fatalf("report context deadline = %v, want within 5s", deadline)
	}
	reportCancel()
	if !errors.Is(reportCtx.Err(), context.Canceled) {
		t.Fatalf(
			"report context error after cancel = %v, want context.Canceled",
			reportCtx.Err(),
		)
	}

	fixedTime := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return fixedTime }
	wait := func(context.Context, time.Duration) error { return nil }
	reporter := reporterFunc(
		func(context.Context, AttemptReport) error { return nil },
	)
	factory := func(
		ctx context.Context,
	) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	}
	injected, err := newRotatorWithDependencies(
		factory,
		WithRotatorClock(clock),
		WithRotatorWait(wait),
		WithAttemptReporter(reporter),
		WithMaxSourceAttempts(16),
		WithRetryDelay(time.Nanosecond),
	)
	if err != nil {
		t.Fatalf("newRotatorWithDependencies() error = %v", err)
	}
	if injected.clock() != fixedTime ||
		reflect.ValueOf(injected.wait).Pointer() !=
			reflect.ValueOf(wait).Pointer() ||
		injected.maxSourceAttempts != 16 ||
		injected.retryDelay != time.Nanosecond ||
		injected.reporter == nil {
		t.Fatalf("injected Rotator = %#v", injected)
	}
}

func TestNewRotator_RejectsInvalidAndDuplicateOptions(t *testing.T) {
	reporter := reporterFunc(
		func(context.Context, AttemptReport) error { return nil },
	)
	var typedNilReporter *pointerReporter
	optionCause := &safeCauseError{
		text: "GET https://user:password@example.invalid/file?token=secret#fragment",
	}
	tests := []struct {
		name    string
		factory reportContextFactory
		options []RotationOption
	}{
		{
			name:    "nil report context factory",
			factory: nil,
		},
		{
			name:    "nil option",
			factory: testReportContext,
			options: []RotationOption{nil},
		},
		{
			name:    "nil clock",
			factory: testReportContext,
			options: []RotationOption{WithRotatorClock(nil)},
		},
		{
			name:    "duplicate clock",
			factory: testReportContext,
			options: []RotationOption{
				WithRotatorClock(time.Now),
				WithRotatorClock(time.Now),
			},
		},
		{
			name:    "nil wait",
			factory: testReportContext,
			options: []RotationOption{WithRotatorWait(nil)},
		},
		{
			name:    "duplicate wait",
			factory: testReportContext,
			options: []RotationOption{
				WithRotatorWait(defaultWait),
				WithRotatorWait(defaultWait),
			},
		},
		{
			name:    "nil reporter",
			factory: testReportContext,
			options: []RotationOption{WithAttemptReporter(nil)},
		},
		{
			name:    "typed nil pointer reporter",
			factory: testReportContext,
			options: []RotationOption{
				WithAttemptReporter(typedNilReporter),
			},
		},
		{
			name:    "typed nil function reporter",
			factory: testReportContext,
			options: []RotationOption{
				WithAttemptReporter(reporterFunc(nil)),
			},
		},
		{
			name:    "duplicate reporter",
			factory: testReportContext,
			options: []RotationOption{
				WithAttemptReporter(reporter),
				WithAttemptReporter(reporter),
			},
		},
		{
			name:    "attempts below boundary",
			factory: testReportContext,
			options: []RotationOption{WithMaxSourceAttempts(0)},
		},
		{
			name:    "attempts above boundary",
			factory: testReportContext,
			options: []RotationOption{WithMaxSourceAttempts(17)},
		},
		{
			name:    "duplicate attempts",
			factory: testReportContext,
			options: []RotationOption{
				WithMaxSourceAttempts(1),
				WithMaxSourceAttempts(2),
			},
		},
		{
			name:    "zero delay",
			factory: testReportContext,
			options: []RotationOption{WithRetryDelay(0)},
		},
		{
			name:    "negative delay",
			factory: testReportContext,
			options: []RotationOption{WithRetryDelay(-time.Nanosecond)},
		},
		{
			name:    "duplicate delay",
			factory: testReportContext,
			options: []RotationOption{
				WithRetryDelay(time.Millisecond),
				WithRetryDelay(time.Second),
			},
		},
		{
			name:    "option returns arbitrary error",
			factory: testReportContext,
			options: []RotationOption{
				func(*rotationOptions) error {
					return optionCause
				},
			},
		},
		{
			name:    "option corrupts final values",
			factory: testReportContext,
			options: []RotationOption{
				func(options *rotationOptions) error {
					options.retryDelay = 0
					return nil
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newRotatorWithDependencies(
				test.factory,
				test.options...,
			)
			if !errors.Is(err, ErrInvalidRotationOption) {
				t.Fatalf(
					"newRotatorWithDependencies() error = %v, want %v",
					err,
					ErrInvalidRotationOption,
				)
			}
			if test.name != "option returns arbitrary error" {
				return
			}
			if err.Error() != "mirror rotation option is invalid" {
				t.Errorf(
					"newRotatorWithDependencies() error = %q, want stable option text",
					err,
				)
			}
			if !errors.Is(err, optionCause) {
				t.Errorf(
					"newRotatorWithDependencies() error = %v, want original cause",
					err,
				)
			}
			var typedCause *safeCauseError
			if !errors.As(err, &typedCause) || typedCause != optionCause {
				t.Errorf(
					"errors.As(*safeCauseError) = %#v, want original cause",
					typedCause,
				)
			}
			for _, forbidden := range []string{
				"user:password",
				"example.invalid",
				"token=secret",
				"fragment",
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf(
						"newRotatorWithDependencies() error = %q, contains %q",
						err,
						forbidden,
					)
				}
			}
		})
	}
}

func TestRotator_HasNoCloseLifecycle(t *testing.T) {
	closer := reflect.TypeOf((*interface{ Close() error })(nil)).Elem()
	if reflect.TypeOf((*Rotator)(nil)).Implements(closer) {
		t.Fatal("*Rotator implements Close, want resource-free lifecycle")
	}
}

type reporterFunc func(context.Context, AttemptReport) error

func (f reporterFunc) ReportAttempt(
	ctx context.Context,
	report AttemptReport,
) error {
	return f(ctx, report)
}

type pointerReporter struct{}

func (*pointerReporter) ReportAttempt(
	context.Context,
	AttemptReport,
) error {
	return nil
}

type safeCauseError struct {
	text string
}

func (e *safeCauseError) Error() string {
	return e.text
}

func testReportContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
