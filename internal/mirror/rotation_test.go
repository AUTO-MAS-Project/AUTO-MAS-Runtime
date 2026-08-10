package mirror

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
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

func TestRotator_RunFirstSourceFirstAttemptSuccess(t *testing.T) {
	plan := mustOnlinePlan(t, KindGit)
	target := mustRotationTarget(t)
	rotator := mustRotator(t)
	actualCommit := strings.Repeat("a", 40)
	var attempts []Attempt

	result, err := rotator.Run(
		context.Background(),
		plan,
		target,
		func(_ context.Context, attempt Attempt) AttemptOutcome {
			attempts = append(attempts, attempt)
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: actualCommit,
			}
		},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Source.Key() != "cnb" ||
		attempts[0].SourceTry != 1 ||
		attempts[0].GlobalTry != 1 ||
		attempts[0].Target != target {
		t.Fatalf("Attempt = %#v", attempts[0])
	}
	if result.Source.Key() != "cnb" ||
		result.ActualCommit != actualCommit ||
		len(result.Reports) != 1 {
		t.Fatalf("RotationResult = %#v", result)
	}
	report := result.Reports[0]
	if report.Outcome != OutcomeSucceeded ||
		report.SourceKey != "cnb" ||
		report.Target != target ||
		report.TargetHash != target.Fingerprint() ||
		report.ActualCommit != actualCommit ||
		report.Duration < 0 {
		t.Fatalf("AttemptReport = %#v", report)
	}
}

func TestRotator_RunInvokesReporter(t *testing.T) {
	reporterCalls := 0
	var reported AttemptReport
	rotator := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(
					ctx context.Context,
					report AttemptReport,
				) error {
					reporterCalls++
					if ctx.Err() != nil {
						t.Errorf("report context error = %v, want nil", ctx.Err())
					}
					reported = report
					return nil
				},
			),
		),
	)
	result, err := rotator.Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("a", 40),
			}
		},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if reporterCalls != 1 ||
		len(result.Reports) != 1 ||
		!reflect.DeepEqual(reported, result.Reports[0]) {
		t.Fatalf(
			"reporter calls/report/result = %d/%#v/%#v",
			reporterCalls,
			reported,
			result.Reports,
		)
	}
}

func TestRotator_RunRetriesSwitchesAndBlacklists(t *testing.T) {
	tests := []struct {
		name          string
		outcomes      []OutcomeKind
		wantKeys      []string
		wantSourceTry []int
		wantWaits     int
	}{
		{
			name:          "same source retry succeeds",
			outcomes:      []OutcomeKind{OutcomeRetrySameSource, OutcomeSucceeded},
			wantKeys:      []string{"cnb", "cnb"},
			wantSourceTry: []int{1, 2},
			wantWaits:     1,
		},
		{
			name: "retry exhaustion moves to next source",
			outcomes: []OutcomeKind{
				OutcomeRetrySameSource,
				OutcomeRetrySameSource,
				OutcomeSucceeded,
			},
			wantKeys:      []string{"cnb", "cnb", "github"},
			wantSourceTry: []int{1, 2, 1},
			wantWaits:     1,
		},
		{
			name:          "switch skips remaining source tries",
			outcomes:      []OutcomeKind{OutcomeSwitchSource, OutcomeSucceeded},
			wantKeys:      []string{"cnb", "github"},
			wantSourceTry: []int{1, 1},
		},
		{
			name: "integrity failure blacklists current source",
			outcomes: []OutcomeKind{
				OutcomeIntegrityFailure,
				OutcomeSucceeded,
			},
			wantKeys:      []string{"cnb", "github"},
			wantSourceTry: []int{1, 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := mustOnlinePlan(t, KindGit)
			target := mustRotationTarget(t)
			waitCalls := 0
			rotator := mustRotator(
				t,
				WithRotatorWait(
					func(_ context.Context, delay time.Duration) error {
						waitCalls++
						if delay != 250*time.Millisecond {
							t.Fatalf("wait delay = %v, want 250ms", delay)
						}
						return nil
					},
				),
			)
			gotKeys := make([]string, 0, len(test.outcomes))
			gotSourceTries := make([]int, 0, len(test.outcomes))
			globalIndex := 0
			result, err := rotator.Run(
				context.Background(),
				plan,
				target,
				func(_ context.Context, attempt Attempt) AttemptOutcome {
					gotKeys = append(gotKeys, attempt.Source.Key())
					gotSourceTries = append(
						gotSourceTries,
						attempt.SourceTry,
					)
					if attempt.GlobalTry != globalIndex+1 {
						t.Fatalf(
							"GlobalTry = %d, want %d",
							attempt.GlobalTry,
							globalIndex+1,
						)
					}
					outcomeKind := test.outcomes[globalIndex]
					globalIndex++
					if outcomeKind == OutcomeSucceeded {
						return AttemptOutcome{
							Kind:         outcomeKind,
							ActualCommit: strings.Repeat("b", 40),
						}
					}
					failureKind := FailureKind("source_unavailable")
					switch outcomeKind {
					case OutcomeRetrySameSource:
						failureKind = FailureKind("network_temporary")
					case OutcomeIntegrityFailure:
						failureKind = FailureKind("integrity")
					}
					return AttemptOutcome{
						Kind:        outcomeKind,
						FailureKind: failureKind,
						Err:         errors.New("classified failure"),
					}
				},
			)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !reflect.DeepEqual(gotKeys, test.wantKeys) {
				t.Fatalf(
					"attempt source keys = %#v, want %#v",
					gotKeys,
					test.wantKeys,
				)
			}
			if !reflect.DeepEqual(gotSourceTries, test.wantSourceTry) {
				t.Fatalf(
					"SourceTry values = %#v, want %#v",
					gotSourceTries,
					test.wantSourceTry,
				)
			}
			if waitCalls != test.wantWaits {
				t.Fatalf("wait calls = %d, want %d", waitCalls, test.wantWaits)
			}
			if len(result.Reports) != len(test.outcomes) {
				t.Fatalf(
					"report count = %d, want %d",
					len(result.Reports),
					len(test.outcomes),
				)
			}
			if test.outcomes[0] == OutcomeIntegrityFailure {
				firstReport := result.Reports[0]
				if firstReport.Outcome != OutcomeIntegrityFailure ||
					firstReport.FailureKind != FailureKind("integrity") {
					t.Fatalf(
						"first integrity report = %#v",
						firstReport,
					)
				}
			}
			if result.Source.Key() != test.wantKeys[len(test.wantKeys)-1] {
				t.Fatalf("result source = %q", result.Source.Key())
			}
		})
	}
}

func TestRotator_RunMapsOfflineAndNaturalExhaustion(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		catalog := mustDefaultCatalog(t)
		policy, err := NewPolicy(PolicySpec{Offline: true})
		if err != nil {
			t.Fatalf("NewPolicy() error = %v", err)
		}
		plan, err := BuildPlan(catalog, policy, KindUV)
		if err != nil {
			t.Fatalf("BuildPlan() error = %v", err)
		}
		attemptCalls := 0
		result, err := mustRotator(t).Run(
			context.Background(),
			plan,
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				return AttemptOutcome{}
			},
		)
		var rotationErr *RotationError
		if !errors.As(err, &rotationErr) ||
			rotationErr.Code() != protocol.CodeNetworkUnavailable {
			t.Fatalf(
				"Run() error = %v, want NETWORK_UNAVAILABLE",
				err,
			)
		}
		if attemptCalls != 0 ||
			len(result.Reports) != 0 ||
			len(rotationErr.Reports()) != 0 {
			t.Fatalf(
				"offline side effects = calls %d/result %#v/error %#v",
				attemptCalls,
				result,
				rotationErr.Reports(),
			)
		}
	})

	t.Run("pre-cancelled offline returns cancellation", func(t *testing.T) {
		catalog := mustDefaultCatalog(t)
		policy, err := NewPolicy(PolicySpec{Offline: true})
		if err != nil {
			t.Fatalf("NewPolicy() error = %v", err)
		}
		plan, err := BuildPlan(catalog, policy, KindUV)
		if err != nil {
			t.Fatalf("BuildPlan() error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attemptCalls := 0
		result, err := mustRotator(t).Run(
			ctx,
			plan,
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				return AttemptOutcome{}
			},
		)
		var rotationErr *RotationError
		if !errors.Is(err, context.Canceled) ||
			errors.As(err, &rotationErr) ||
			err.Error() != "mirror operation cancelled" {
			t.Fatalf(
				"Run() error = %T %v, want safe cancellation",
				err,
				err,
			)
		}
		if attemptCalls != 0 ||
			len(result.Reports) != 0 {
			t.Fatalf(
				"pre-cancelled offline side effects = calls %d/result %#v",
				attemptCalls,
				result,
			)
		}
	})

	t.Run("pure source exhaustion", func(t *testing.T) {
		causeOne := errors.New("first transport failure")
		causeTwo := errors.New("second transport failure")
		causes := []error{causeOne, causeTwo}
		index := 0
		result, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				outcome := AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         causes[index],
				}
				index++
				return outcome
			},
		)
		var rotationErr *RotationError
		if !errors.As(err, &rotationErr) ||
			rotationErr.Code() != protocol.CodeMirrorExhausted {
			t.Fatalf("Run() error = %v, want MIRROR_EXHAUSTED", err)
		}
		if !errors.Is(err, causeOne) || !errors.Is(err, causeTwo) {
			t.Fatalf("Run() error = %v, want both transport causes", err)
		}
		if len(result.Reports) != 2 ||
			!reflect.DeepEqual(result.Reports, rotationErr.Reports()) {
			t.Fatalf(
				"result/error reports = %#v/%#v",
				result.Reports,
				rotationErr.Reports(),
			)
		}
	})

	t.Run("integrity dominates natural exhaustion", func(t *testing.T) {
		integrityCause := errors.New("checksum mismatch")
		transportCause := errors.New("next source unavailable")
		index := 0
		result, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				index++
				if index == 1 {
					return AttemptOutcome{
						Kind:        OutcomeIntegrityFailure,
						FailureKind: FailureKind("integrity"),
						Err:         integrityCause,
					}
				}
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         transportCause,
				}
			},
		)
		var integrityErr *IntegrityExhaustedError
		var rotationErr *RotationError
		if !errors.As(err, &integrityErr) || errors.As(err, &rotationErr) {
			t.Fatalf(
				"Run() error = %T %v, want IntegrityExhaustedError",
				err,
				err,
			)
		}
		if !errors.Is(err, integrityCause) ||
			!errors.Is(err, transportCause) ||
			len(result.Reports) != 2 ||
			len(integrityErr.Reports()) != 2 {
			t.Fatalf("integrity exhaustion lost reports or causes")
		}
	})
}

func TestRotator_TargetFailureStopsAndPreservesCause(t *testing.T) {
	targetCause := errors.New("release branch does not exist")
	attemptCalls := 0
	result, err := mustRotator(t).Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			attemptCalls++
			return AttemptOutcome{
				Kind:        OutcomeTargetFailure,
				FailureKind: FailureKind("target"),
				Err:         targetCause,
			}
		},
	)
	var integrityErr *IntegrityExhaustedError
	if !errors.Is(err, targetCause) || errors.As(err, &integrityErr) {
		t.Fatalf("Run() error = %v, want target cause only", err)
	}
	if attemptCalls != 1 ||
		len(result.Reports) != 1 ||
		result.Reports[0].Outcome != OutcomeTargetFailure {
		t.Fatalf(
			"target failure state = calls %d/result %#v",
			attemptCalls,
			result,
		)
	}
}

func TestRotator_TargetAndFingerprintStayIdenticalAcrossAttempts(t *testing.T) {
	plan := mustOnlinePlan(t, KindGit)
	target := mustRotationTarget(t)
	actualCommit := strings.Repeat("c", 40)
	var attempts []Attempt
	rotator := mustRotator(
		t,
		WithRotatorWait(
			func(context.Context, time.Duration) error { return nil },
		),
	)
	result, err := rotator.Run(
		context.Background(),
		plan,
		target,
		func(_ context.Context, attempt Attempt) AttemptOutcome {
			attempts = append(attempts, attempt)
			if len(attempts) == 1 {
				return AttemptOutcome{
					Kind:        OutcomeRetrySameSource,
					FailureKind: FailureKind("network_temporary"),
					Err:         errors.New("temporary"),
				}
			}
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: actualCommit,
			}
		},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(attempts) != 2 || attempts[0].Target != attempts[1].Target {
		t.Fatalf("Attempt targets = %#v", attempts)
	}
	for _, attempt := range attempts {
		if attempt.Target != target ||
			attempt.Target.Fingerprint() != target.Fingerprint() {
			t.Fatalf("Attempt target changed: %#v", attempt.Target)
		}
	}
	if result.Reports[0].ActualCommit != "" ||
		result.Reports[1].ActualCommit != actualCommit ||
		result.ActualCommit != actualCommit {
		t.Fatalf("ActualCommit propagation = %#v", result)
	}
}

func TestRotator_FailureKindDoesNotAffectTransitions(t *testing.T) {
	failureKinds := []FailureKind{
		"network_temporary",
		"connect_timeout",
		"checksum_mismatch",
		"future_transport_reason",
	}
	outcomes := []OutcomeKind{
		OutcomeRetrySameSource,
		OutcomeSwitchSource,
		OutcomeIntegrityFailure,
		OutcomeTargetFailure,
	}
	type observation struct {
		keys           []string
		sourceTries    []int
		reportOutcomes []OutcomeKind
		waitCalls      int
		resultSource   string
		terminal       bool
	}
	fullTraversal := map[Kind][]string{
		KindGit: {"cnb", "github"},
		KindUV: {
			"agentsmirror",
			"gh-proxy",
			"cdn-gh-proxy",
			"edgeone-gh-proxy",
			"github",
		},
		KindPython:       {"gh-proxy", "github"},
		KindPackageIndex: {"aliyun", "tsinghua", "ustc", "pypi"},
	}

	for _, kind := range AllKinds() {
		for _, outcomeKind := range outcomes {
			t.Run(
				kind.String()+"/"+outcomeKind.String(),
				func(t *testing.T) {
					var baseline observation
					for index, failureKind := range failureKinds {
						waitCalls := 0
						var keys []string
						var sourceTries []int
						attemptCalls := 0
						result, err := mustRotator(
							t,
							WithRotatorWait(
								func(context.Context, time.Duration) error {
									waitCalls++
									return nil
								},
							),
						).Run(
							context.Background(),
							mustOnlinePlan(t, kind),
							mustRotationTarget(t),
							func(
								_ context.Context,
								attempt Attempt,
							) AttemptOutcome {
								attemptCalls++
								keys = append(keys, attempt.Source.Key())
								sourceTries = append(
									sourceTries,
									attempt.SourceTry,
								)
								if outcomeKind == OutcomeSwitchSource {
									if !attempt.Source.Official() {
										return AttemptOutcome{
											Kind:        OutcomeSwitchSource,
											FailureKind: failureKind,
											Err:         errors.New("classified failure"),
										}
									}
									success := AttemptOutcome{
										Kind: OutcomeSucceeded,
									}
									if kind == KindGit {
										success.ActualCommit = strings.Repeat("d", 40)
									}
									return success
								}
								if attemptCalls == 1 {
									return AttemptOutcome{
										Kind:        outcomeKind,
										FailureKind: failureKind,
										Err:         errors.New("classified failure"),
									}
								}
								success := AttemptOutcome{
									Kind: OutcomeSucceeded,
								}
								if kind == KindGit {
									success.ActualCommit = strings.Repeat("d", 40)
								}
								return success
							},
						)
						terminal := outcomeKind == OutcomeTargetFailure
						if terminal != (err != nil) {
							t.Fatalf(
								"Run(%q) error = %v, terminal %t",
								failureKind,
								err,
								terminal,
							)
						}
						if outcomeKind == OutcomeSwitchSource {
							wantKeys := fullTraversal[kind]
							if !reflect.DeepEqual(keys, wantKeys) ||
								result.Source.Key() != wantKeys[len(wantKeys)-1] ||
								!result.Source.Official() {
								t.Fatalf(
									"switch traversal = keys %#v/source %#v, want %#v/final official",
									keys,
									result.Source,
									wantKeys,
								)
							}
						}
						reportOutcomes := make(
							[]OutcomeKind,
							len(result.Reports),
						)
						for i, report := range result.Reports {
							reportOutcomes[i] = report.Outcome
						}
						got := observation{
							keys:           keys,
							sourceTries:    sourceTries,
							reportOutcomes: reportOutcomes,
							waitCalls:      waitCalls,
							resultSource:   result.Source.Key(),
							terminal:       terminal,
						}
						if index == 0 {
							baseline = got
							continue
						}
						if !reflect.DeepEqual(got, baseline) {
							t.Fatalf(
								"FailureKind %q changed transition: got %#v, want %#v",
								failureKind,
								got,
								baseline,
							)
						}
					}
				},
			)
		}
	}
}

func TestRotator_RejectsInvalidAttemptOutcome(t *testing.T) {
	secretErr := &safeCauseError{
		text: "GET https://user:password@example.invalid/file?token=secret#fragment",
	}
	validCommit := strings.Repeat("d", 40)
	tests := []struct {
		name    string
		kind    Kind
		outcome AttemptOutcome
	}{
		{
			name: "unknown outcome",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:        OutcomeKind("unknown"),
				FailureKind: FailureKind("source_unavailable"),
				Err:         secretErr,
			},
		},
		{
			name: "callback forges cancelled",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:        OutcomeCancelled,
				FailureKind: FailureKind("target"),
				Err:         context.Canceled,
			},
		},
		{
			name: "success has error",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: validCommit,
				Err:          secretErr,
			},
		},
		{
			name: "success has failure kind",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				FailureKind:  FailureKind("network_temporary"),
				ActualCommit: validCommit,
			},
		},
		{
			name:    "git success lacks actual commit",
			kind:    KindGit,
			outcome: AttemptOutcome{Kind: OutcomeSucceeded},
		},
		{
			name: "git commit is 39 characters",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("a", 39),
			},
		},
		{
			name: "git commit is 41 characters",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("a", 41),
			},
		},
		{
			name: "git commit is uppercase",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("A", 40),
			},
		},
		{
			name: "git commit has lowercase non hex",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("a", 39) + "g",
			},
		},
		{
			name: "non git success has actual commit",
			kind: KindUV,
			outcome: AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: validCommit,
			},
		},
		{
			name: "failure lacks error",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("source_unavailable"),
			},
		},
		{
			name: "failure lacks failure kind",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind: OutcomeSwitchSource,
				Err:  secretErr,
			},
		},
		{
			name: "failure carries actual commit",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:         OutcomeSwitchSource,
				FailureKind:  FailureKind("source_unavailable"),
				ActualCommit: validCommit,
				Err:          secretErr,
			},
		},
		{
			name: "failure kind is uppercase",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:        OutcomeRetrySameSource,
				FailureKind: FailureKind("CONNECT_TIMEOUT"),
				Err:         secretErr,
			},
		},
		{
			name: "failure kind has consecutive underscores",
			kind: KindGit,
			outcome: AttemptOutcome{
				Kind:        OutcomeTargetFailure,
				FailureKind: FailureKind("target__changed"),
				Err:         secretErr,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := mustRotator(
				t,
				WithMaxSourceAttempts(1),
			).Run(
				context.Background(),
				mustOnlinePlan(t, test.kind),
				mustRotationTarget(t),
				func(context.Context, Attempt) AttemptOutcome {
					return test.outcome
				},
			)
			if !errors.Is(err, ErrInvalidRotationRequest) {
				t.Fatalf(
					"Run() error = %v, want ErrInvalidRotationRequest",
					err,
				)
			}
			if len(result.Reports) != 1 {
				t.Fatalf("report count = %d, want 1", len(result.Reports))
			}
			if test.outcome.Err != nil &&
				!errors.Is(err, test.outcome.Err) {
				t.Fatalf("Run() error lost callback cause: %v", err)
			}
			for _, forbidden := range []string{
				"user:password",
				"example.invalid",
				"token=secret",
				"fragment",
			} {
				if strings.Contains(err.Error(), forbidden) ||
					strings.Contains(result.Reports[0].Error, forbidden) {
					t.Fatalf(
						"invalid outcome leaked %q: error %q/report %q",
						forbidden,
						err,
						result.Reports[0].Error,
					)
				}
			}
			if test.outcome.Kind == OutcomeCancelled &&
				result.Reports[0].Outcome == OutcomeCancelled {
				t.Fatal("callback forged a cancelled report")
			}
			if result.Source != (Source{}) || result.ActualCommit != "" {
				t.Fatalf("invalid outcome populated success: %#v", result)
			}
		})
	}
}

func TestRotator_SucceededOutcomeForEveryKind(t *testing.T) {
	tests := []struct {
		kind   Kind
		commit string
		keys   []string
	}{
		{
			kind:   KindGit,
			commit: strings.Repeat("a", 40),
			keys:   []string{"cnb", "github"},
		},
		{
			kind: KindUV,
			keys: []string{
				"agentsmirror",
				"gh-proxy",
				"cdn-gh-proxy",
				"edgeone-gh-proxy",
				"github",
			},
		},
		{
			kind: KindPython,
			keys: []string{"gh-proxy", "github"},
		},
		{
			kind: KindPackageIndex,
			keys: []string{"aliyun", "tsinghua", "ustc", "pypi"},
		},
	}
	for _, test := range tests {
		t.Run(test.kind.String(), func(t *testing.T) {
			var attemptedKeys []string
			result, err := mustRotator(t).Run(
				context.Background(),
				mustOnlinePlan(t, test.kind),
				mustRotationTarget(t),
				func(_ context.Context, attempt Attempt) AttemptOutcome {
					attemptedKeys = append(
						attemptedKeys,
						attempt.Source.Key(),
					)
					if !attempt.Source.Official() {
						return AttemptOutcome{
							Kind:        OutcomeSwitchSource,
							FailureKind: FailureKind("source_unavailable"),
							Err:         errors.New("mirror unavailable"),
						}
					}
					return AttemptOutcome{
						Kind:         OutcomeSucceeded,
						ActualCommit: test.commit,
					}
				},
			)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !reflect.DeepEqual(attemptedKeys, test.keys) ||
				result.Source.Key() != test.keys[len(test.keys)-1] ||
				!result.Source.Official() ||
				result.ActualCommit != test.commit ||
				len(result.Reports) != len(test.keys) ||
				result.Reports[len(result.Reports)-1].Outcome != OutcomeSucceeded ||
				result.Reports[len(result.Reports)-1].ActualCommit != test.commit {
				t.Fatalf(
					"Run() attempts/result = %#v/%#v, want keys %#v/final official",
					attemptedKeys,
					result,
					test.keys,
				)
			}
			for index, report := range result.Reports[:len(result.Reports)-1] {
				if report.Outcome != OutcomeSwitchSource ||
					report.SourceKey != test.keys[index] ||
					report.ActualCommit != "" {
					t.Fatalf("Run() report[%d] = %#v", index, report)
				}
			}
		})
	}
}

func TestRotator_CancellationStopsAtEveryCoreBoundary(t *testing.T) {
	t.Run("before first attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attemptCalls := 0
		result, err := mustRotator(t).Run(
			ctx,
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				return AttemptOutcome{}
			},
		)
		if !errors.Is(err, context.Canceled) ||
			attemptCalls != 0 ||
			len(result.Reports) != 0 {
			t.Fatalf(
				"Run() = result %#v/error %v/calls %d",
				result,
				err,
				attemptCalls,
			)
		}
	})

	t.Run("during attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		waitCalls := 0
		result, err := mustRotator(
			t,
			WithRotatorWait(
				func(context.Context, time.Duration) error {
					waitCalls++
					return nil
				},
			),
		).Run(
			ctx,
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				cancel()
				return AttemptOutcome{
					Kind:        OutcomeRetrySameSource,
					FailureKind: FailureKind("network_temporary"),
					Err:         errors.New("interrupted"),
				}
			},
		)
		if !errors.Is(err, context.Canceled) || waitCalls != 0 {
			t.Fatalf("Run() error = %v/waits %d", err, waitCalls)
		}
		if len(result.Reports) != 1 ||
			result.Reports[0].Outcome != OutcomeCancelled {
			t.Fatalf("cancelled report = %#v", result.Reports)
		}
	})

	t.Run("during retry wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attemptCalls := 0
		result, err := mustRotator(
			t,
			WithRotatorWait(
				func(waitCtx context.Context, _ time.Duration) error {
					cancel()
					return waitCtx.Err()
				},
			),
		).Run(
			ctx,
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				return AttemptOutcome{
					Kind:        OutcomeRetrySameSource,
					FailureKind: FailureKind("network_temporary"),
					Err:         errors.New("temporary"),
				}
			},
		)
		if !errors.Is(err, context.Canceled) || attemptCalls != 1 {
			t.Fatalf("Run() error = %v/calls %d", err, attemptCalls)
		}
		if len(result.Reports) != 1 ||
			result.Reports[0].Outcome != OutcomeRetrySameSource {
			t.Fatalf("wait cancellation reports = %#v", result.Reports)
		}
	})

	t.Run("legal success wins simultaneous cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		result, err := mustRotator(t).Run(
			ctx,
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				cancel()
				return AttemptOutcome{
					Kind:         OutcomeSucceeded,
					ActualCommit: strings.Repeat("e", 40),
				}
			},
		)
		if err != nil ||
			result.Source.Key() != "cnb" ||
			result.Reports[0].Outcome != OutcomeSucceeded {
			t.Fatalf("Run() = result %#v/error %v", result, err)
		}
	})
}

func TestRotator_PostReportCancellationStopsBeforeSwitchSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attemptCalls := 0
	var attemptedKeys []string
	reporterCalls := 0
	result, err := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(context.Context, AttemptReport) error {
					reporterCalls++
					cancel()
					return nil
				},
			),
		),
	).Run(
		ctx,
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(_ context.Context, attempt Attempt) AttemptOutcome {
			attemptCalls++
			attemptedKeys = append(attemptedKeys, attempt.Source.Key())
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("source_unavailable"),
				Err:         errors.New("source unavailable"),
			}
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if attemptCalls != 1 ||
		reporterCalls != 1 ||
		!reflect.DeepEqual(attemptedKeys, []string{"cnb"}) ||
		len(result.Reports) != 1 ||
		result.Reports[0].Outcome != OutcomeSwitchSource {
		t.Fatalf(
			"switch cancellation = attempts %d/keys %#v/reporter %d/reports %#v",
			attemptCalls,
			attemptedKeys,
			reporterCalls,
			result.Reports,
		)
	}
}

func TestRotator_InvalidRequestHasNoObservableSideEffects(t *testing.T) {
	validPlan := mustOnlinePlan(t, KindGit)
	validTarget := mustRotationTarget(t)
	forgedPlan := validPlan
	forgedPlan.seal = [32]byte{}
	uvOnlyTarget, err := NewTarget(TargetSpec{UVVersion: "0.8.12"})
	if err != nil {
		t.Fatalf("NewTarget() error = %v", err)
	}
	var nilCtx context.Context

	tests := []struct {
		name    string
		rotator func(*Rotator) *Rotator
		ctx     context.Context
		plan    Plan
		target  Target
		attempt bool
	}{
		{
			name:    "nil receiver",
			rotator: func(*Rotator) *Rotator { return nil },
			ctx:     context.Background(),
			plan:    validPlan,
			target:  validTarget,
			attempt: true,
		},
		{
			name:    "nil context",
			ctx:     nilCtx,
			plan:    validPlan,
			target:  validTarget,
			attempt: true,
		},
		{
			name:   "nil attempt",
			ctx:    context.Background(),
			plan:   validPlan,
			target: validTarget,
		},
		{
			name:    "zero plan",
			ctx:     context.Background(),
			target:  validTarget,
			attempt: true,
		},
		{
			name:    "forged plan",
			ctx:     context.Background(),
			plan:    forgedPlan,
			target:  validTarget,
			attempt: true,
		},
		{
			name:    "zero target",
			ctx:     context.Background(),
			plan:    validPlan,
			attempt: true,
		},
		{
			name:    "target missing git fields",
			ctx:     context.Background(),
			plan:    validPlan,
			target:  uvOnlyTarget,
			attempt: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clockCalls := 0
			waitCalls := 0
			attemptCalls := 0
			rotator := mustRotator(
				t,
				WithRotatorClock(func() time.Time {
					clockCalls++
					return time.Unix(0, 0)
				}),
				WithRotatorWait(
					func(context.Context, time.Duration) error {
						waitCalls++
						return nil
					},
				),
			)
			if test.rotator != nil {
				rotator = test.rotator(rotator)
			}
			var attempt AttemptFunc
			if test.attempt {
				attempt = func(context.Context, Attempt) AttemptOutcome {
					attemptCalls++
					return AttemptOutcome{
						Kind:         OutcomeSucceeded,
						ActualCommit: strings.Repeat("f", 40),
					}
				}
			}
			result, err := rotator.Run(
				test.ctx,
				test.plan,
				test.target,
				attempt,
			)
			if !errors.Is(err, ErrInvalidRotationRequest) {
				t.Fatalf(
					"Run() error = %v, want ErrInvalidRotationRequest",
					err,
				)
			}
			if clockCalls != 0 || waitCalls != 0 || attemptCalls != 0 {
				t.Fatalf(
					"side effects = clock %d/wait %d/attempt %d",
					clockCalls,
					waitCalls,
					attemptCalls,
				)
			}
			if !reflect.DeepEqual(result, RotationResult{}) {
				t.Fatalf("Run() result = %#v, want zero", result)
			}
		})
	}
}

func TestRotator_UsesOutcomeNotErrorTextForDecisions(t *testing.T) {
	t.Run("integrity words remain transport exhaustion", func(t *testing.T) {
		_, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err: errors.New(
						"integrity_failure checksum mismatch",
					),
				}
			},
		)
		var rotationErr *RotationError
		var integrityErr *IntegrityExhaustedError
		if !errors.As(err, &rotationErr) || errors.As(err, &integrityErr) {
			t.Fatalf("Run() error type = %T, want RotationError", err)
		}
	})

	t.Run("retry words remain integrity exhaustion", func(t *testing.T) {
		_, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				return AttemptOutcome{
					Kind:        OutcomeIntegrityFailure,
					FailureKind: FailureKind("integrity"),
					Err: errors.New(
						"retry_same_source connection timeout",
					),
				}
			},
		)
		var integrityErr *IntegrityExhaustedError
		if !errors.As(err, &integrityErr) {
			t.Fatalf(
				"Run() error type = %T, want IntegrityExhaustedError",
				err,
			)
		}
	})
}

func TestRotator_ErrorTextDoesNotLeakSensitiveCauses(t *testing.T) {
	const secretText = "GET https://user:password@example.invalid/file?token=secret#fragment"
	tests := []struct {
		name        string
		wantText    string
		wantSuccess bool
		run         func(*testing.T, error) (RotationResult, error)
	}{
		{
			name:     "callback contract",
			wantText: "mirror attempt contract is invalid",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(t).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:         OutcomeSwitchSource,
							FailureKind:  FailureKind("source_unavailable"),
							ActualCommit: strings.Repeat("6", 40),
							Err:          cause,
						}
					},
				)
			},
		},
		{
			name:     "target",
			wantText: "mirror target failed",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(t).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:        OutcomeTargetFailure,
							FailureKind: FailureKind("target"),
							Err:         cause,
						}
					},
				)
			},
		},
		{
			name:     "wait",
			wantText: "mirror retry wait failed",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(
					t,
					WithRotatorWait(
						func(context.Context, time.Duration) error {
							return cause
						},
					),
				).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:        OutcomeRetrySameSource,
							FailureKind: FailureKind("network_temporary"),
							Err:         errors.New("classified failure"),
						}
					},
				)
			},
		},
		{
			name:     "reporter",
			wantText: "mirror attempt reporting failed",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(
					t,
					WithAttemptReporter(
						reporterFunc(
							func(context.Context, AttemptReport) error {
								return cause
							},
						),
					),
				).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:        OutcomeSwitchSource,
							FailureKind: FailureKind("source_unavailable"),
							Err:         errors.New("classified failure"),
						}
					},
				)
			},
		},
		{
			name:        "success reporter",
			wantText:    "mirror attempt reporting failed",
			wantSuccess: true,
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(
					t,
					WithAttemptReporter(
						reporterFunc(
							func(context.Context, AttemptReport) error {
								return cause
							},
						),
					),
				).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:         OutcomeSucceeded,
							ActualCommit: strings.Repeat("7", 40),
						}
					},
				)
			},
		},
		{
			name:     "ordinary exhaustion",
			wantText: "mirror sources exhausted",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(
					t,
					WithMaxSourceAttempts(1),
				).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:        OutcomeSwitchSource,
							FailureKind: FailureKind("source_unavailable"),
							Err:         cause,
						}
					},
				)
			},
		},
		{
			name:     "integrity exhaustion",
			wantText: "mirror integrity checks exhausted",
			run: func(t *testing.T, cause error) (RotationResult, error) {
				return mustRotator(
					t,
					WithMaxSourceAttempts(1),
				).Run(
					context.Background(),
					mustOnlinePlan(t, KindGit),
					mustRotationTarget(t),
					func(context.Context, Attempt) AttemptOutcome {
						return AttemptOutcome{
							Kind:        OutcomeIntegrityFailure,
							FailureKind: FailureKind("integrity"),
							Err:         cause,
						}
					},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New(secretText)
			result, err := test.run(t, cause)
			if err == nil {
				t.Fatal("Run() error = nil, want failure")
			}
			if err.Error() != test.wantText {
				t.Fatalf(
					"Run() Error() = %q, want %q",
					err.Error(),
					test.wantText,
				)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("Run() error lost cause: %v", err)
			}
			if len(result.Reports) == 0 {
				t.Fatal("Run() returned no report for an attempted endpoint")
			}
			if test.wantSuccess &&
				(result.Source.Key() == "" || result.ActualCommit == "") {
				t.Fatalf("success reporter result = %#v", result)
			}
			for _, forbidden := range []string{
				"user:password",
				"example.invalid",
				"token=secret",
				"fragment",
				"https://",
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf(
						"Run() Error() = %q, contains %q",
						err,
						forbidden,
					)
				}
			}
		})
	}
}

func TestRotator_ErrorTextURLHTTPBoundaryUsesSafeWrapper(t *testing.T) {
	transportCause := &safeCauseError{
		text: "dial tcp leaked-boundary-cause",
	}
	rawURLError := &url.Error{
		Op:  "Get",
		URL: "https://user:password@example.invalid/file?token=secret#fragment",
		Err: transportCause,
	}
	result, err := mustRotator(
		t,
		WithMaxSourceAttempts(1),
	).Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("network"),
				Err:         rawURLError,
			}
		},
	)
	var rotationErr *RotationError
	var gotURLError *url.Error
	if !errors.As(err, &rotationErr) ||
		!errors.As(err, &gotURLError) ||
		gotURLError != rawURLError ||
		!errors.Is(err, transportCause) {
		t.Fatalf(
			"Run() error = %v, want safe RotationError preserving *url.Error and typed cause",
			err,
		)
	}
	if err.Error() != "mirror sources exhausted" {
		t.Fatalf(
			"Run() Error() = %q, want mirror sources exhausted",
			err.Error(),
		)
	}
	if len(result.Reports) != 2 ||
		!reflect.DeepEqual(result.Reports, rotationErr.Reports()) {
		t.Fatalf(
			"result/error reports = %#v/%#v",
			result.Reports,
			rotationErr.Reports(),
		)
	}
	publicTexts := append(
		[]string{err.Error()},
		reportErrors(result.Reports)...,
	)
	for _, cause := range rotationErr.Unwrap() {
		publicTexts = append(publicTexts, cause.Error())
	}
	for _, publicText := range publicTexts {
		for _, forbidden := range []string{
			"user:password",
			"example.invalid",
			"token=secret",
			"fragment",
			"leaked-boundary-cause",
			"https://",
		} {
			if strings.Contains(publicText, forbidden) {
				t.Fatalf(
					"public error text %q contains %q",
					publicText,
					forbidden,
				)
			}
		}
	}
}

func TestRotator_ErrorTextReporterAndWaitFailuresPreserveCauseChain(t *testing.T) {
	assertChain := func(
		t *testing.T,
		err error,
		wantText string,
		wantCauses ...error,
	) {
		t.Helper()
		if err == nil || err.Error() != wantText {
			t.Fatalf("terminal error = %v, want %q", err, wantText)
		}
		for _, wantCause := range wantCauses {
			if !errors.Is(err, wantCause) {
				t.Errorf("terminal error lost cause %p: %v", wantCause, err)
			}
		}
		var typedCause *safeCauseError
		if !errors.As(err, &typedCause) || typedCause == nil {
			t.Fatalf("errors.As(*safeCauseError) = %#v, want typed cause", typedCause)
		}
		var outer *safeError
		if !errors.As(err, &outer) || outer == nil {
			t.Fatalf("errors.As(*safeError) = %#v, want outer safe error", outer)
		}
		publicTexts := []string{err.Error()}
		for _, cause := range outer.Unwrap() {
			publicTexts = append(publicTexts, cause.Error())
		}
		for _, publicText := range publicTexts {
			for _, forbidden := range []string{
				"previous-secret",
				"current-secret",
				"terminal-secret",
				"https://",
			} {
				if strings.Contains(publicText, forbidden) {
					t.Errorf(
						"public error text %q contains %q",
						publicText,
						forbidden,
					)
				}
			}
		}
	}

	t.Run("reporter", func(t *testing.T) {
		previousCause := &safeCauseError{
			text: "https://previous-secret.invalid/callback",
		}
		currentCause := &safeCauseError{
			text: "https://current-secret.invalid/callback",
		}
		reporterCause := &safeCauseError{
			text: "https://terminal-secret.invalid/reporter",
		}
		attemptCalls := 0
		reporterCalls := 0
		result, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
			WithAttemptReporter(
				reporterFunc(
					func(_ context.Context, report AttemptReport) error {
						reporterCalls++
						if report.GlobalTry == 2 {
							return reporterCause
						}
						return nil
					},
				),
			),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				cause := previousCause
				if attemptCalls == 2 {
					cause = currentCause
				}
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         cause,
				}
			},
		)
		assertChain(
			t,
			err,
			"mirror attempt reporting failed",
			previousCause,
			currentCause,
			reporterCause,
		)
		if attemptCalls != 2 ||
			reporterCalls != 2 ||
			len(result.Reports) != 2 {
			t.Fatalf(
				"reporter terminal calls/reports = %d/%d/%d, want 2/2/2",
				attemptCalls,
				reporterCalls,
				len(result.Reports),
			)
		}
	})

	t.Run("wait", func(t *testing.T) {
		previousCause := &safeCauseError{
			text: "https://previous-secret.invalid/callback",
		}
		currentCause := &safeCauseError{
			text: "https://current-secret.invalid/callback",
		}
		waitCause := &safeCauseError{
			text: "https://terminal-secret.invalid/wait",
		}
		attemptCalls := 0
		waitCalls := 0
		result, err := mustRotator(
			t,
			WithRotatorWait(
				func(context.Context, time.Duration) error {
					waitCalls++
					return waitCause
				},
			),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				if attemptCalls == 1 {
					return AttemptOutcome{
						Kind:        OutcomeSwitchSource,
						FailureKind: FailureKind("source_unavailable"),
						Err:         previousCause,
					}
				}
				return AttemptOutcome{
					Kind:        OutcomeRetrySameSource,
					FailureKind: FailureKind("network_temporary"),
					Err:         currentCause,
				}
			},
		)
		assertChain(
			t,
			err,
			"mirror retry wait failed",
			previousCause,
			currentCause,
			waitCause,
		)
		if attemptCalls != 2 ||
			waitCalls != 1 ||
			len(result.Reports) != 2 {
			t.Fatalf(
				"wait terminal calls/reports = %d/%d/%d, want 2/1/2",
				attemptCalls,
				waitCalls,
				len(result.Reports),
			)
		}
	})
}

func reportErrors(reports []AttemptReport) []string {
	errors := make([]string, len(reports))
	for index, report := range reports {
		errors[index] = report.Error
	}
	return errors
}

func mustOnlinePlan(t *testing.T, kind Kind) Plan {
	t.Helper()
	policy, err := NewPolicy(PolicySpec{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := BuildPlan(mustDefaultCatalog(t), policy, kind)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	return plan
}

func mustRotationTarget(t *testing.T) Target {
	t.Helper()
	target, err := NewTarget(TargetSpec{
		ProductVersion: "v5.3.0",
		ReleaseBranch:  "release/v5.3.0",
		UVVersion:      "0.8.12",
		PythonVersion:  "3.12.10",
		LockDigest:     strings.Repeat("a1", 32),
	})
	if err != nil {
		t.Fatalf("NewTarget() error = %v", err)
	}
	return target
}

func mustRotator(t *testing.T, options ...RotationOption) *Rotator {
	t.Helper()
	rotator, err := NewRotator(options...)
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	return rotator
}

func TestRotator_ReportsEveryAttemptWithIndependentContext(t *testing.T) {
	type contextKey struct{}
	type reportContextIDKey struct{}
	operationCtx := context.WithValue(
		context.Background(),
		contextKey{},
		"operation-value",
	)
	operationCtx, cancelOperation := context.WithCancel(operationCtx)
	factoryCalls := 0
	cancelCalls := make([]int, 0, 2)
	reporterCalls := 0
	var reportContexts []context.Context
	var reported []AttemptReport
	factory := func(
		ctx context.Context,
	) (context.Context, context.CancelFunc) {
		id := factoryCalls
		factoryCalls++
		baseCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		reportCtx := context.WithValue(baseCtx, reportContextIDKey{}, id)
		cancelCalls = append(cancelCalls, 0)
		return reportCtx, func() {
			cancelCalls[id]++
			cancel()
		}
	}
	reporter := reporterFunc(
		func(ctx context.Context, report AttemptReport) error {
			if ctx.Err() != nil {
				t.Errorf("report context error = %v, want nil", ctx.Err())
			}
			if got := ctx.Value(contextKey{}); got != "operation-value" {
				t.Errorf("report context value = %#v, want operation-value", got)
			}
			if got := ctx.Value(reportContextIDKey{}); got != reporterCalls {
				t.Errorf(
					"report context id = %#v, want %d",
					got,
					reporterCalls,
				)
			}
			reporterCalls++
			reportContexts = append(reportContexts, ctx)
			reported = append(reported, report)
			return nil
		},
	)
	rotator, err := newRotatorWithDependencies(
		factory,
		WithMaxSourceAttempts(1),
		WithAttemptReporter(reporter),
	)
	if err != nil {
		t.Fatalf("newRotatorWithDependencies() error = %v", err)
	}
	result, err := rotator.Run(
		operationCtx,
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			if reporterCalls == 1 {
				cancelOperation()
			}
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("source_unavailable"),
				Err:         errors.New("operation interrupted"),
			}
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if reporterCalls != 2 {
		t.Fatalf("reporter calls = %d, want 2", reporterCalls)
	}
	if factoryCalls != 2 ||
		!reflect.DeepEqual(cancelCalls, []int{1, 1}) {
		t.Fatalf(
			"report context lifecycle = factory %d/cancels %#v, want 2/[1 1]",
			factoryCalls,
			cancelCalls,
		)
	}
	if len(reportContexts) != 2 ||
		reportContexts[0] == reportContexts[1] {
		t.Fatalf("report contexts are not two distinct instances: %#v", reportContexts)
	}
	for index, reportCtx := range reportContexts {
		if !errors.Is(reportCtx.Err(), context.Canceled) {
			t.Errorf(
				"report context %d error after cancel = %v",
				index,
				reportCtx.Err(),
			)
		}
	}
	if len(result.Reports) != 2 ||
		!reflect.DeepEqual(reported, result.Reports) ||
		reported[0].Outcome != OutcomeSwitchSource ||
		reported[1].Outcome != OutcomeCancelled {
		t.Fatalf(
			"reported/result reports = %#v/%#v",
			reported,
			result.Reports,
		)
	}
}

func TestRotator_PostReportCancellationPrecedesTargetFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	targetErr := errors.New(
		"GET https://user:password@example.invalid/release?token=secret",
	)
	attemptCalls := 0
	reporterCalls := 0
	result, err := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(context.Context, AttemptReport) error {
					reporterCalls++
					cancel()
					return nil
				},
			),
		),
	).Run(
		ctx,
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			attemptCalls++
			return AttemptOutcome{
				Kind:        OutcomeTargetFailure,
				FailureKind: FailureKind("target"),
				Err:         targetErr,
			}
		},
	)
	if !errors.Is(err, context.Canceled) ||
		!errors.Is(err, targetErr) ||
		err.Error() != "mirror operation cancelled" {
		t.Fatalf(
			"Run() error = %v, want safe cancellation to precede target failure",
			err,
		)
	}
	if attemptCalls != 1 ||
		reporterCalls != 1 ||
		len(result.Reports) != 1 {
		t.Fatalf(
			"calls/reports = attempt %d/reporter %d/reports %d",
			attemptCalls,
			reporterCalls,
			len(result.Reports),
		)
	}
	for _, forbidden := range []string{
		"user:password",
		"example.invalid",
		"token=secret",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("Run() Error() = %q, contains %q", err, forbidden)
		}
	}
}

func TestRotator_ReporterFailureStopsFurtherAttempts(t *testing.T) {
	reporterErr := errors.New(
		"POST https://user:password@example.invalid/report?token=secret",
	)
	attemptCalls := 0
	reporterCalls := 0
	result, err := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(context.Context, AttemptReport) error {
					reporterCalls++
					return reporterErr
				},
			),
		),
	).Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			attemptCalls++
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("source_unavailable"),
				Err:         errors.New("source unavailable"),
			}
		},
	)
	var rotationErr *RotationError
	if !errors.Is(err, reporterErr) ||
		errors.As(err, &rotationErr) ||
		err.Error() != "mirror attempt reporting failed" {
		t.Fatalf("Run() error = %v, want safe reporter error", err)
	}
	if attemptCalls != 1 || reporterCalls != 1 || len(result.Reports) != 1 {
		t.Fatalf(
			"calls/reports = attempt %d/reporter %d/reports %d",
			attemptCalls,
			reporterCalls,
			len(result.Reports),
		)
	}
}

func TestRotator_SuccessReporterFailureReturnsPopulatedResult(t *testing.T) {
	reporterErr := errors.New(
		"POST https://user:password@example.invalid/report?token=secret",
	)
	actualCommit := strings.Repeat("1", 40)
	var reporterCopy AttemptReport
	result, err := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(_ context.Context, report AttemptReport) error {
					reporterCopy = report
					report.SourceKey = "mutated-by-reporter"
					return reporterErr
				},
			),
		),
	).Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: actualCommit,
			}
		},
	)
	if !errors.Is(err, reporterErr) ||
		err.Error() != "mirror attempt reporting failed" {
		t.Fatalf("Run() error = %v, want safe reporter error", err)
	}
	if result.Source.Key() != "cnb" ||
		result.ActualCommit != actualCommit ||
		len(result.Reports) != 1 ||
		result.Reports[0].SourceKey != "cnb" ||
		reporterCopy.SourceKey != "cnb" {
		t.Fatalf(
			"success with reporter failure result = %#v/reporter %#v",
			result,
			reporterCopy,
		)
	}
}

func TestRotator_CancellationAndReporterErrorAreBothPreserved(t *testing.T) {
	reporterErr := errors.New(
		"POST https://user:password@example.invalid/report?token=secret#fragment",
	)
	tests := []struct {
		name          string
		cancelAttempt bool
		cancelReport  bool
	}{
		{name: "cancelled before reporter", cancelAttempt: true},
		{name: "cancelled by reporter", cancelReport: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			reporterCalls := 0
			attemptCalls := 0
			result, err := mustRotator(
				t,
				WithAttemptReporter(
					reporterFunc(
						func(
							reportCtx context.Context,
							report AttemptReport,
						) error {
							reporterCalls++
							if reportCtx.Err() != nil {
								t.Errorf(
									"report context error = %v",
									reportCtx.Err(),
								)
							}
							if test.cancelReport {
								cancel()
							}
							if report.GlobalTry != 1 {
								t.Errorf(
									"report GlobalTry = %d, want 1",
									report.GlobalTry,
								)
							}
							return reporterErr
						},
					),
				),
			).Run(
				ctx,
				mustOnlinePlan(t, KindGit),
				mustRotationTarget(t),
				func(context.Context, Attempt) AttemptOutcome {
					attemptCalls++
					if test.cancelAttempt {
						cancel()
					}
					return AttemptOutcome{
						Kind:        OutcomeSwitchSource,
						FailureKind: FailureKind("source_unavailable"),
						Err:         errors.New("attempt failed"),
					}
				},
			)
			if !errors.Is(err, context.Canceled) ||
				!errors.Is(err, reporterErr) ||
				err.Error() != "mirror operation cancelled" {
				t.Fatalf(
					"Run() error = %v, want safe cancellation and reporter error",
					err,
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
						"Run() Error() = %q, contains %q",
						err,
						forbidden,
					)
				}
			}
			if attemptCalls != 1 ||
				reporterCalls != 1 ||
				len(result.Reports) != 1 {
				t.Fatalf(
					"calls/reports = %d/%d/%d",
					attemptCalls,
					reporterCalls,
					len(result.Reports),
				)
			}
			if test.cancelAttempt &&
				result.Reports[0].Outcome != OutcomeCancelled {
				t.Fatalf(
					"pre-report cancellation outcome = %q",
					result.Reports[0].Outcome,
				)
			}
		})
	}
}

func TestRotator_LegalSuccessWinsReporterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	actualCommit := strings.Repeat("2", 40)
	result, err := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(context.Context, AttemptReport) error {
					cancel()
					return nil
				},
			),
		),
	).Run(
		ctx,
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: actualCommit,
			}
		},
	)
	if err != nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Run() error = %v/context error = %v", err, ctx.Err())
	}
	if result.Source.Key() != "cnb" ||
		result.ActualCommit != actualCommit ||
		result.Reports[0].Outcome != OutcomeSucceeded {
		t.Fatalf("success result = %#v", result)
	}
}

func TestRotator_IntegrityHistoryDoesNotOverrideLaterTerminal(t *testing.T) {
	integrityErr := errors.New("first source checksum mismatch")

	t.Run("later target failure", func(t *testing.T) {
		targetErr := errors.New("target cannot be satisfied")
		attemptCalls := 0
		_, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				if attemptCalls == 1 {
					return AttemptOutcome{
						Kind:        OutcomeIntegrityFailure,
						FailureKind: FailureKind("integrity"),
						Err:         integrityErr,
					}
				}
				return AttemptOutcome{
					Kind:        OutcomeTargetFailure,
					FailureKind: FailureKind("target"),
					Err:         targetErr,
				}
			},
		)
		var exhaustedErr *IntegrityExhaustedError
		if !errors.Is(err, targetErr) ||
			errors.As(err, &exhaustedErr) ||
			attemptCalls != 2 {
			t.Fatalf(
				"Run() error = %v/calls %d, want target terminal",
				err,
				attemptCalls,
			)
		}
	})

	t.Run("later reporter failure", func(t *testing.T) {
		reporterErr := errors.New("reporter unavailable")
		attemptCalls := 0
		reporterCalls := 0
		_, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
			WithAttemptReporter(
				reporterFunc(
					func(context.Context, AttemptReport) error {
						reporterCalls++
						if reporterCalls == 2 {
							return reporterErr
						}
						return nil
					},
				),
			),
		).Run(
			context.Background(),
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				if attemptCalls == 1 {
					return AttemptOutcome{
						Kind:        OutcomeIntegrityFailure,
						FailureKind: FailureKind("integrity"),
						Err:         integrityErr,
					}
				}
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         errors.New("second source failed"),
				}
			},
		)
		var exhaustedErr *IntegrityExhaustedError
		if !errors.Is(err, reporterErr) ||
			errors.As(err, &exhaustedErr) ||
			attemptCalls != 2 {
			t.Fatalf(
				"Run() error = %v/calls %d, want reporter terminal",
				err,
				attemptCalls,
			)
		}
	})

	t.Run("later cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attemptCalls := 0
		result, err := mustRotator(
			t,
			WithMaxSourceAttempts(1),
		).Run(
			ctx,
			mustOnlinePlan(t, KindGit),
			mustRotationTarget(t),
			func(context.Context, Attempt) AttemptOutcome {
				attemptCalls++
				if attemptCalls == 1 {
					return AttemptOutcome{
						Kind:        OutcomeIntegrityFailure,
						FailureKind: FailureKind("integrity"),
						Err:         integrityErr,
					}
				}
				cancel()
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         errors.New("cancelled transport"),
				}
			},
		)
		var exhaustedErr *IntegrityExhaustedError
		if !errors.Is(err, context.Canceled) ||
			errors.As(err, &exhaustedErr) ||
			attemptCalls != 2 {
			t.Fatalf(
				"Run() error = %v/calls %d, want cancellation",
				err,
				attemptCalls,
			)
		}
		if len(result.Reports) != 2 ||
			result.Reports[1].Outcome != OutcomeCancelled {
			t.Fatalf("cancellation reports = %#v", result.Reports)
		}
	})
}

func TestRotator_ReportSequenceUsesInjectedClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	times := []time.Time{
		base,
		base.Add(5 * time.Millisecond),
		base.Add(10 * time.Millisecond),
		base.Add(17 * time.Millisecond),
	}
	clockIndex := 0
	var reported []AttemptReport
	rotator := mustRotator(
		t,
		WithMaxSourceAttempts(1),
		WithRotatorClock(func() time.Time {
			current := times[clockIndex]
			clockIndex++
			return current
		}),
		WithAttemptReporter(
			reporterFunc(
				func(_ context.Context, report AttemptReport) error {
					reported = append(reported, report)
					return nil
				},
			),
		),
	)
	attemptCalls := 0
	result, err := rotator.Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			attemptCalls++
			if attemptCalls == 1 {
				return AttemptOutcome{
					Kind:        OutcomeSwitchSource,
					FailureKind: FailureKind("source_unavailable"),
					Err:         errors.New("first source unavailable"),
				}
			}
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("3", 40),
			}
		},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(reported, result.Reports) {
		t.Fatalf(
			"reported/result reports = %#v/%#v",
			reported,
			result.Reports,
		)
	}
	wantDurations := []time.Duration{5 * time.Millisecond, 7 * time.Millisecond}
	wantKeys := []string{"cnb", "github"}
	for index, report := range result.Reports {
		if report.StartedAt != times[index*2] ||
			report.Duration != wantDurations[index] ||
			report.SourceKey != wantKeys[index] ||
			report.SourceTry != 1 ||
			report.GlobalTry != index+1 {
			t.Fatalf("report[%d] = %#v", index, report)
		}
	}
}

func TestRotator_ConcurrentRunsIsolateCountersAndBlacklist(t *testing.T) {
	rotator := mustRotator(t, WithMaxSourceAttempts(1))
	plan := mustOnlinePlan(t, KindGit)
	target := mustRotationTarget(t)
	runABlockedOnNextSource := make(chan struct{})
	releaseRunA := make(chan struct{})
	var releaseOnce sync.Once
	releaseA := func() {
		releaseOnce.Do(func() { close(releaseRunA) })
	}
	t.Cleanup(releaseA)

	type runResult struct {
		result   RotationResult
		err      error
		attempts []Attempt
	}
	runAResult := make(chan runResult, 1)
	go func() {
		attempts := make([]Attempt, 0, 2)
		result, err := rotator.Run(
			context.Background(),
			plan,
			target,
			func(_ context.Context, attempt Attempt) AttemptOutcome {
				attempts = append(attempts, attempt)
				if len(attempts) == 1 {
					return AttemptOutcome{
						Kind:        OutcomeIntegrityFailure,
						FailureKind: FailureKind("integrity"),
						Err:         errors.New("run A integrity failure"),
					}
				}
				close(runABlockedOnNextSource)
				<-releaseRunA
				return AttemptOutcome{
					Kind:         OutcomeSucceeded,
					ActualCommit: strings.Repeat("4", 40),
				}
			},
		)
		runAResult <- runResult{
			result:   result,
			err:      err,
			attempts: attempts,
		}
	}()

	select {
	case <-runABlockedOnNextSource:
	case <-time.After(5 * time.Second):
		t.Fatal("Run A did not blacklist CNB and reach the next source")
	}

	var runBAttempts []Attempt
	runB, runBErr := rotator.Run(
		context.Background(),
		plan,
		target,
		func(_ context.Context, attempt Attempt) AttemptOutcome {
			runBAttempts = append(runBAttempts, attempt)
			return AttemptOutcome{
				Kind:         OutcomeSucceeded,
				ActualCommit: strings.Repeat("5", 40),
			}
		},
	)
	if runBErr != nil {
		t.Fatalf("Run B error = %v", runBErr)
	}
	if len(runBAttempts) != 1 ||
		runBAttempts[0].Source.Key() != "cnb" ||
		runBAttempts[0].SourceTry != 1 ||
		runBAttempts[0].GlobalTry != 1 ||
		runB.Source.Key() != "cnb" {
		t.Fatalf(
			"Run B observed Run A blacklist or counters: attempts %#v/result %#v",
			runBAttempts,
			runB,
		)
	}

	releaseA()
	var runA runResult
	select {
	case runA = <-runAResult:
	case <-time.After(5 * time.Second):
		t.Fatal("Run A did not return after release")
	}
	if runA.err != nil ||
		runA.result.Source.Key() != "github" ||
		len(runA.attempts) != 2 ||
		runA.attempts[0].Source.Key() != "cnb" ||
		runA.attempts[1].Source.Key() != "github" ||
		runA.attempts[1].GlobalTry != 2 ||
		runA.attempts[1].SourceTry != 1 {
		t.Fatalf(
			"Run A state = result %#v/error %v/attempts %#v",
			runA.result,
			runA.err,
			runA.attempts,
		)
	}
}

// 上述 barrier 先证明 Run A 已把 CNB 写入自己的 blacklist 并进入 github callback，
// 才启动 Run B。若把 blacklist 移到共享 Rotator（即使用 mutex 消除 race），
// Run B 会跳过 CNB，精确触发 “observed Run A blacklist” 失败。

func TestRotator_AttemptReportRedactsSecretBearingErrors(t *testing.T) {
	secretErr := errors.New(
		"GET https://user:password@example.invalid/file?token=top-secret",
	)
	result, err := mustRotator(
		t,
		WithMaxSourceAttempts(1),
	).Run(
		context.Background(),
		mustOnlinePlan(t, KindGit),
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: FailureKind("source_unavailable"),
				Err:         secretErr,
			}
		},
	)
	var rotationErr *RotationError
	if !errors.As(err, &rotationErr) ||
		!errors.Is(err, secretErr) {
		t.Fatalf("Run() error = %v, want preserved RotationError cause", err)
	}
	for _, report := range result.Reports {
		for _, forbidden := range []string{
			"user:password",
			"example.invalid",
			"token=top-secret",
			"https://",
		} {
			if strings.Contains(report.Error, forbidden) {
				t.Fatalf(
					"AttemptReport.Error = %q, contains %q",
					report.Error,
					forbidden,
				)
			}
		}
	}
	for _, forbidden := range []string{
		"user:password",
		"example.invalid",
		"token=top-secret",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("Run() Error() = %q, contains %q", err, forbidden)
		}
	}
}

func TestRotator_InvalidRequestDoesNotInvokeReporter(t *testing.T) {
	reporterCalls := 0
	rotator := mustRotator(
		t,
		WithAttemptReporter(
			reporterFunc(
				func(context.Context, AttemptReport) error {
					reporterCalls++
					return nil
				},
			),
		),
	)
	_, err := rotator.Run(
		context.Background(),
		Plan{},
		mustRotationTarget(t),
		func(context.Context, Attempt) AttemptOutcome {
			t.Fatal("AttemptFunc called for invalid request")
			return AttemptOutcome{}
		},
	)
	if !errors.Is(err, ErrInvalidRotationRequest) {
		t.Fatalf("Run() error = %v, want ErrInvalidRotationRequest", err)
	}
	if reporterCalls != 0 {
		t.Fatalf("reporter calls = %d, want 0", reporterCalls)
	}
}
