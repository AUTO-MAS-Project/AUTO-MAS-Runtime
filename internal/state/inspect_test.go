package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakePIDProbe struct {
	alive bool
	err   error
	calls int
	pid   uint32
}

func (p *fakePIDProbe) Alive(
	_ context.Context,
	pid uint32,
) (bool, error) {
	p.calls++
	p.pid = pid
	return p.alive, p.err
}

type fakeMutexProbe struct {
	result MutexProbeResult
	err    error
	calls  int
	kind   MutexKind
}

func (p *fakeMutexProbe) Probe(
	_ context.Context,
	kind MutexKind,
) (MutexProbeResult, error) {
	p.calls++
	p.kind = kind
	return p.result, p.err
}

func TestInspectTransaction_ClassifiesActivity(t *testing.T) {
	t.Parallel()

	mutexCause := errors.New("mutex probe failed")
	pidCause := errors.New("pid probe failed")
	tests := []struct {
		name       string
		mutex      MutexProbeResult
		mutexErr   error
		pidAlive   bool
		pidErr     error
		want       Activity
		pidChecked bool
		wantErr    error
	}{
		{
			name:       "active",
			mutex:      MutexProbeResult{Held: true},
			pidAlive:   false,
			want:       ActivityActive,
			pidChecked: false,
		},
		{
			name:       "stale",
			mutex:      MutexProbeResult{},
			pidAlive:   false,
			want:       ActivityStale,
			pidChecked: true,
		},
		{
			name:       "inconsistent",
			mutex:      MutexProbeResult{},
			pidAlive:   true,
			want:       ActivityInconsistent,
			pidChecked: true,
		},
		{
			name:       "unknown_mutex",
			mutexErr:   mutexCause,
			want:       ActivityUnknown,
			pidChecked: false,
			wantErr:    mutexCause,
		},
		{
			name:       "unknown_pid",
			mutex:      MutexProbeResult{},
			pidErr:     pidCause,
			want:       ActivityUnknown,
			pidChecked: true,
			wantErr:    pidCause,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			pidProbe := &fakePIDProbe{
				alive: test.pidAlive,
				err:   test.pidErr,
			}
			mutexProbe := &fakeMutexProbe{
				result: test.mutex,
				err:    test.mutexErr,
			}
			got := InspectTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
				pidProbe,
				mutexProbe,
			)
			if got.Activity != test.want {
				t.Fatalf("Activity = %q, want %q", got.Activity, test.want)
			}
			if got.PIDChecked != test.pidChecked {
				t.Fatalf(
					"PIDChecked = %t, want %t",
					got.PIDChecked,
					test.pidChecked,
				)
			}
			if got.PIDAlive != (test.pidChecked && test.pidAlive && test.pidErr == nil) {
				t.Fatalf("PIDAlive = %t, unexpected probe projection", got.PIDAlive)
			}
			if test.wantErr != nil && !errors.Is(got.ProbeError, test.wantErr) {
				t.Fatalf("ProbeError = %v, want %v", got.ProbeError, test.wantErr)
			}
		})
	}
}

func TestInspectTransaction_MutexHeldSkipsPIDProbe(t *testing.T) {
	t.Parallel()

	pidProbe := &fakePIDProbe{err: errors.New("must not be called")}
	mutexProbe := &fakeMutexProbe{
		result: MutexProbeResult{Held: true},
	}
	got := InspectTransaction(
		t.Context(),
		TransactionBackend,
		validTransactionState(TransactionBackend),
		pidProbe,
		mutexProbe,
	)
	if got.Activity != ActivityActive || !got.MutexHeld {
		t.Fatalf("inspection = %#v, want active held", got)
	}
	if pidProbe.calls != 0 || got.PIDChecked {
		t.Fatalf("PID calls/checked = %d/%t, want 0/false", pidProbe.calls, got.PIDChecked)
	}
}

func TestInspectTransaction_MutexHeldWinsOverCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	pidProbe := &fakePIDProbe{err: errors.New("must not be called")}
	mutexProbe := &cancelingMutexProbe{
		cancel: cancel,
		result: MutexProbeResult{Held: true},
	}
	got := InspectTransaction(
		ctx,
		TransactionMutation,
		validTransactionState(TransactionMutation),
		pidProbe,
		mutexProbe,
	)
	if ctx.Err() == nil {
		t.Fatal("context error = nil, want canceled by mutex probe")
	}
	if got.Activity != ActivityActive ||
		!got.MutexHeld ||
		got.ProbeError != nil {
		t.Fatalf("inspection = %#v, want active held without probe error", got)
	}
	if pidProbe.calls != 0 || got.PIDChecked {
		t.Fatalf("PID calls/checked = %d/%t, want 0/false", pidProbe.calls, got.PIDChecked)
	}
}

func TestInspectTransaction_BindsMutexToTransactionKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind TransactionKind
		want MutexKind
	}{
		{kind: TransactionBackend, want: MutexBackend},
		{kind: TransactionMutation, want: MutexMutation},
		{kind: TransactionUpdate, want: MutexMutation},
	}
	for _, test := range tests {
		test := test
		t.Run(test.kind.String(), func(t *testing.T) {
			pidProbe := &fakePIDProbe{}
			mutexProbe := &fakeMutexProbe{
				result: MutexProbeResult{Held: true},
			}
			got := InspectTransaction(
				t.Context(),
				test.kind,
				validTransactionState(test.kind),
				pidProbe,
				mutexProbe,
			)
			if got.Activity != ActivityActive {
				t.Fatalf("Activity = %q, want active", got.Activity)
			}
			if mutexProbe.calls != 1 || mutexProbe.kind != test.want {
				t.Fatalf(
					"Mutex probe = %d/%q, want 1/%q",
					mutexProbe.calls,
					mutexProbe.kind,
					test.want,
				)
			}
		})
	}
}

func TestInspectTransaction_InvalidInputSkipsAllProbes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ctx        context.Context
		kind       TransactionKind
		value      TransactionState
		pidProbe   PIDProbe
		mutexProbe MutexProbe
	}{
		{
			name:       "nil_context",
			kind:       TransactionBackend,
			value:      validTransactionState(TransactionBackend),
			pidProbe:   &fakePIDProbe{},
			mutexProbe: &fakeMutexProbe{},
		},
		{
			name:       "unknown_kind",
			ctx:        t.Context(),
			kind:       TransactionKind("future"),
			value:      validTransactionState(TransactionBackend),
			pidProbe:   &fakePIDProbe{},
			mutexProbe: &fakeMutexProbe{},
		},
		{
			name: "invalid_value",
			ctx:  t.Context(),
			kind: TransactionBackend,
			value: func() TransactionState {
				value := validTransactionState(TransactionBackend)
				value.PID = 0
				return value
			}(),
			pidProbe:   &fakePIDProbe{},
			mutexProbe: &fakeMutexProbe{},
		},
		{
			name:       "nil_pid_probe",
			ctx:        t.Context(),
			kind:       TransactionBackend,
			value:      validTransactionState(TransactionBackend),
			mutexProbe: &fakeMutexProbe{},
		},
		{
			name:     "nil_mutex_probe",
			ctx:      t.Context(),
			kind:     TransactionBackend,
			value:    validTransactionState(TransactionBackend),
			pidProbe: &fakePIDProbe{},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			got := InspectTransaction(
				test.ctx,
				test.kind,
				test.value,
				test.pidProbe,
				test.mutexProbe,
			)
			if got.Activity != ActivityUnknown || got.ProbeError == nil {
				t.Fatalf("inspection = %#v, want unknown with error", got)
			}
			if probe, ok := test.pidProbe.(*fakePIDProbe); ok && probe.calls != 0 {
				t.Fatalf("PID calls = %d, want 0", probe.calls)
			}
			if probe, ok := test.mutexProbe.(*fakeMutexProbe); ok && probe.calls != 0 {
				t.Fatalf("Mutex calls = %d, want 0", probe.calls)
			}
		})
	}
}

func TestInspectTransaction_ProbeFailureFailsClosed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	mutexProbe := &cancelingMutexProbe{cancel: cancel}
	pidProbe := &fakePIDProbe{alive: true}
	got := InspectTransaction(
		ctx,
		TransactionMutation,
		validTransactionState(TransactionMutation),
		pidProbe,
		mutexProbe,
	)
	if got.Activity != ActivityUnknown ||
		!errors.Is(got.ProbeError, context.Canceled) {
		t.Fatalf("inspection = %#v, want canceled unknown", got)
	}
	if pidProbe.calls != 0 {
		t.Fatalf("PID calls = %d, want 0 after cancellation", pidProbe.calls)
	}
}

type cancelingMutexProbe struct {
	cancel context.CancelFunc
	result MutexProbeResult
}

func (p *cancelingMutexProbe) Probe(
	context.Context,
	MutexKind,
) (MutexProbeResult, error) {
	p.cancel()
	return p.result, nil
}

func TestInspectTransaction_PreservesRecoveredProbe(t *testing.T) {
	t.Parallel()

	pidCause := errors.New("pid unavailable")
	tests := []struct {
		name     string
		alive    bool
		err      error
		activity Activity
	}{
		{name: "dead", activity: ActivityStale},
		{name: "alive", alive: true, activity: ActivityInconsistent},
		{name: "error", err: pidCause, activity: ActivityUnknown},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			got := InspectTransaction(
				t.Context(),
				TransactionUpdate,
				validTransactionState(TransactionUpdate),
				&fakePIDProbe{alive: test.alive, err: test.err},
				&fakeMutexProbe{
					result: MutexProbeResult{Recovered: true},
				},
			)
			if got.Activity != test.activity || !got.MutexRecovered {
				t.Fatalf(
					"inspection = %#v, want %q with recovered",
					got,
					test.activity,
				)
			}
		})
	}
}

func TestInspectTransaction_StartedAtAndPIDReuseDoNotChangeTruth(t *testing.T) {
	t.Parallel()

	for _, startedAt := range []time.Time{
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		value := validTransactionState(TransactionBackend)
		value.StartedAt = startedAt
		got := InspectTransaction(
			t.Context(),
			TransactionBackend,
			value,
			&fakePIDProbe{alive: true},
			&fakeMutexProbe{},
		)
		if got.Activity != ActivityInconsistent {
			t.Fatalf(
				"StartedAt %v Activity = %q, want inconsistent",
				startedAt,
				got.Activity,
			)
		}
	}
}

func TestInspection_OnlyStaleCanAutoClean(t *testing.T) {
	t.Parallel()

	for _, activity := range []Activity{
		ActivityActive,
		ActivityStale,
		ActivityInconsistent,
		ActivityUnknown,
	} {
		got := (Inspection{Activity: activity}).CanAutoClean()
		if got != (activity == ActivityStale) {
			t.Fatalf("CanAutoClean(%q) = %t", activity, got)
		}
	}
}
