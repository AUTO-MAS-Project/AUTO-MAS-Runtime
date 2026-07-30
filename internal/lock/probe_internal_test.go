//go:build windows

package lock

import (
	"context"
	"errors"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

type probeCallResult struct {
	result ProbeResult
	err    error
}

func TestSet_ProbeMapsWaitResults(t *testing.T) {
	injectedErr := errors.New("injected probe wait failure")
	tests := []struct {
		name        string
		waitResult  uint32
		waitErr     error
		want        ProbeResult
		wantRelease int
		wantError   bool
	}{
		{
			name:        "free",
			waitResult:  waitResultObject0,
			want:        ProbeResult{},
			wantRelease: 1,
		},
		{
			name:        "abandoned",
			waitResult:  waitResultAbandoned,
			want:        ProbeResult{Recovered: true},
			wantRelease: 1,
		},
		{
			name:       "held",
			waitResult: waitResultTimeout,
			want:       ProbeResult{Held: true},
		},
		{
			name:       "failed",
			waitResult: waitResultFailed,
			waitErr:    injectedErr,
			wantError:  true,
		},
		{
			name:       "unexpected",
			waitResult: 0x42,
			wantError:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = func(apiCall) (uint32, error) {
				return test.waitResult, test.waitErr
			}
			set := newWorkerTestSet(t, api)
			result, err := set.Probe(t.Context(), KindBackend)
			if test.wantError && err == nil {
				t.Fatal("Probe() error = nil, want failure")
			}
			if !test.wantError && err != nil {
				t.Fatalf("Probe() error = %v", err)
			}
			if result != test.want {
				t.Fatalf("Probe() = %#v, want %#v", result, test.want)
			}
			if got := countHandleCalls(
				api,
				"release",
				testBackendHandle,
			); got != test.wantRelease {
				t.Fatalf(
					"Release calls = %d, want %d",
					got,
					test.wantRelease,
				)
			}
		})
	}
}

func TestSet_ProbeChecksContextBeforeQueueWaitAndReply(t *testing.T) {
	t.Run("before queue", func(t *testing.T) {
		api := newTestWindowsAPI()
		set := newWorkerTestSet(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result, err := set.Probe(ctx, KindBackend)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Probe() error = %v, want context.Canceled", err)
		}
		if result != (ProbeResult{}) || api.count("wait") != 0 {
			t.Fatalf(
				"Probe/Wait calls = %#v/%d, want zero/0",
				result,
				api.count("wait"),
			)
		}
	})

	t.Run("before worker Wait", func(t *testing.T) {
		api := newTestWindowsAPI()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		state := newAcquireWorkerState(api)
		response := state.probe(workerRequest{
			ctx:  ctx,
			kind: KindBackend,
		})
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("probe() error = %v, want context.Canceled", response.err)
		}
		if api.count("wait") != 0 {
			t.Fatalf("Wait calls = %d, want 0", api.count("wait"))
		}
	})

	for _, failure := range []releaseFailure{
		{name: "Release success"},
		{name: "Release failure", handle: testBackendHandle},
	} {
		t.Run("before reply/"+failure.name, func(t *testing.T) {
			runProbeReplyCancellation(t, failure.handle)
		})
	}
}

func runProbeReplyCancellation(
	t *testing.T,
	failingHandle windows.Handle,
) {
	t.Helper()
	injectedErr := errors.New("injected probe release failure")
	api := newTestWindowsAPI()
	if failingHandle != 0 {
		api.releaseErr = func(call apiCall) error {
			if call.Handle == failingHandle {
				return injectedErr
			}
			return nil
		}
	}
	releaseReturned := make(chan struct{})
	allowReply := make(chan struct{})
	var once sync.Once
	api.afterCall = func(call apiCall) {
		if call.Operation == "release" &&
			call.Handle == testBackendHandle {
			once.Do(func() { close(releaseReturned) })
			<-allowReply
		}
	}
	set := newWorkerTestSet(t, api)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultCh := make(chan probeCallResult, 1)
	go func() {
		result, err := set.Probe(ctx, KindBackend)
		resultCh <- probeCallResult{result: result, err: err}
	}()
	waitSignal(t, releaseReturned, "Probe Release return")
	assertPending(t, resultCh, "Probe")
	cancel()
	close(allowReply)
	call := waitValue(t, resultCh, "Probe reply cancellation")
	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("Probe() error = %v, want context.Canceled", call.err)
	}
	if failingHandle != 0 && !errors.Is(call.err, injectedErr) {
		t.Fatalf("Probe() error = %v, want release failure", call.err)
	}
	if call.result != (ProbeResult{}) {
		t.Fatalf("Probe() = %#v, want frozen free result", call.result)
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != 1 {
		t.Fatalf("Release calls = %d, want 1", got)
	}
}

func TestSet_ProbeChecksContextAfterEveryWaitResult(t *testing.T) {
	injectedWaitErr := errors.New("injected probe wait failure")
	for _, wait := range acquireWaitCases(injectedWaitErr) {
		failures := []releaseFailure{{name: "Release success"}}
		if wait.owned {
			failures = append(failures, releaseFailure{
				name:   "Release failure",
				handle: testBackendHandle,
			})
		}
		for _, failure := range failures {
			t.Run(wait.name+"/"+failure.name, func(t *testing.T) {
				runCanceledProbeWait(t, wait, failure.handle)
			})
		}
	}
}

func runCanceledProbeWait(
	t *testing.T,
	wait frozenWait,
	failingHandle windows.Handle,
) {
	t.Helper()
	injectedReleaseErr := errors.New("injected probe release failure")
	api := newTestWindowsAPI()
	waitReturned := make(chan struct{})
	allowWorker := make(chan struct{})
	var once sync.Once
	api.waitResult = func(call apiCall) (uint32, error) {
		if call.Handle != testBackendHandle {
			return waitResultObject0, nil
		}
		once.Do(func() { close(waitReturned) })
		<-allowWorker
		return wait.result, wait.err
	}
	if failingHandle != 0 {
		api.releaseErr = func(call apiCall) error {
			if call.Handle == failingHandle {
				return injectedReleaseErr
			}
			return nil
		}
	}
	set := newWorkerTestSet(t, api)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultCh := make(chan probeCallResult, 1)
	go func() {
		result, err := set.Probe(ctx, KindBackend)
		resultCh <- probeCallResult{result: result, err: err}
	}()
	waitSignal(t, waitReturned, "Probe Wait return")
	cancel()
	close(allowWorker)
	call := waitValue(t, resultCh, "canceled Probe")

	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("Probe() error = %v, want context.Canceled", call.err)
	}
	if failingHandle != 0 && !errors.Is(call.err, injectedReleaseErr) {
		t.Fatalf("Probe() error = %v, want release failure", call.err)
	}
	want := ProbeResult{}
	if wait.recovered {
		want.Recovered = true
	}
	if call.result != want {
		t.Fatalf("Probe() = %#v, want %#v", call.result, want)
	}
	if got := countHandleCalls(
		api,
		"wait",
		testBackendHandle,
	); got != 1 {
		t.Fatalf("Wait calls = %d, want 1", got)
	}
	wantRelease := 0
	if wait.owned {
		wantRelease = 1
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != wantRelease {
		t.Fatalf("Release calls = %d, want %d", got, wantRelease)
	}
}

func TestSet_ProbeHeldByOwnWorkerSkipsWait(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	acquired, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	before := api.count("wait")
	result, err := set.Probe(t.Context(), KindBackend)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if result != (ProbeResult{Held: true}) {
		t.Fatalf("Probe() = %#v, want Held", result)
	}
	if got := api.count("wait"); got != before {
		t.Fatalf("Wait calls = %d, want unchanged %d", got, before)
	}
	if err := acquired.Lease().Close(); err != nil {
		t.Fatalf("Lease.Close() error = %v", err)
	}
}

func TestSet_ProbeReleaseFailurePreservesResultAndPoisons(t *testing.T) {
	injectedErr := errors.New("injected Probe Release failure")
	tests := []struct {
		name       string
		waitResult uint32
		want       ProbeResult
	}{
		{
			name:       "free",
			waitResult: waitResultObject0,
			want:       ProbeResult{},
		},
		{
			name:       "abandoned",
			waitResult: waitResultAbandoned,
			want:       ProbeResult{Recovered: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = func(apiCall) (uint32, error) {
				return test.waitResult, nil
			}
			api.releaseErr = func(call apiCall) error {
				if call.Handle == testBackendHandle {
					return injectedErr
				}
				return nil
			}
			set := newWorkerTestSet(t, api)
			result, err := set.Probe(t.Context(), KindBackend)
			if !errors.Is(err, injectedErr) ||
				!errors.Is(err, ErrPoisoned) {
				t.Fatalf("Probe() error = %v, want poison and cause", err)
			}
			if result != test.want {
				t.Fatalf("Probe() = %#v, want %#v", result, test.want)
			}
			beforeWait := api.count("wait")
			beforeRelease := api.count("release")
			if _, err := set.Probe(
				t.Context(),
				KindMutation,
			); !errors.Is(err, ErrPoisoned) {
				t.Fatalf("poisoned Probe() error = %v, want ErrPoisoned", err)
			}
			if api.count("wait") != beforeWait ||
				api.count("release") != beforeRelease {
				t.Fatalf(
					"poisoned Wait/Release = %d/%d, want %d/%d",
					api.count("wait"),
					api.count("release"),
					beforeWait,
					beforeRelease,
				)
			}
		})
	}
}

func TestSet_ProbeRejectsInvalidKindClosedAndPoisoned(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		api := newTestWindowsAPI()
		set := newWorkerTestSet(t, api)
		if _, err := set.Probe(nil, KindBackend); err == nil {
			t.Fatal("Probe(nil) error = nil, want rejection")
		}
		if api.count("wait") != 0 {
			t.Fatalf("Wait calls = %d, want 0", api.count("wait"))
		}
	})

	t.Run("invalid kind", func(t *testing.T) {
		api := newTestWindowsAPI()
		set := newWorkerTestSet(t, api)
		if _, err := set.Probe(t.Context(), "unknown"); err == nil {
			t.Fatal("Probe(unknown) error = nil, want rejection")
		}
		if api.count("wait") != 0 {
			t.Fatalf("Wait calls = %d, want 0", api.count("wait"))
		}
	})

	t.Run("closed", func(t *testing.T) {
		api := newTestWindowsAPI()
		set := newWorkerTestSet(t, api)
		if err := set.Close(); err != nil {
			t.Fatalf("Set.Close() error = %v", err)
		}
		if _, err := set.Probe(
			t.Context(),
			KindBackend,
		); !errors.Is(err, ErrClosed) {
			t.Fatalf("Probe() error = %v, want ErrClosed", err)
		}
	})

	t.Run("poisoned", func(t *testing.T) {
		injectedErr := errors.New("injected Probe Release failure")
		api := newTestWindowsAPI()
		api.releaseErr = func(call apiCall) error {
			if call.Handle == testBackendHandle {
				return injectedErr
			}
			return nil
		}
		set := newWorkerTestSet(t, api)
		if _, err := set.Probe(
			t.Context(),
			KindBackend,
		); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("first Probe() error = %v, want ErrPoisoned", err)
		}
		before := api.count("wait")
		if _, err := set.Probe(
			t.Context(),
			KindMutation,
		); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("second Probe() error = %v, want ErrPoisoned", err)
		}
		if got := api.count("wait"); got != before {
			t.Fatalf("Wait calls = %d, want unchanged %d", got, before)
		}
	})
}

func testProbeOperationErrors(t *testing.T) {
	t.Helper()
	injectedErr := errors.New("injected Probe operation failure")
	tests := []struct {
		name      string
		operation string
		run       func() error
	}{
		{
			name:      "wait-probe",
			operation: "wait-probe",
			run: func() error {
				api := newTestWindowsAPI()
				api.waitResult = func(apiCall) (uint32, error) {
					return waitResultFailed, injectedErr
				}
				set := newWorkerTestSet(t, api)
				_, err := set.Probe(t.Context(), KindBackend)
				return err
			},
		},
		{
			name:      "release-probe",
			operation: "release-probe",
			run: func() error {
				api := newTestWindowsAPI()
				api.releaseErr = func(call apiCall) error {
					if call.Handle == testBackendHandle {
						return injectedErr
					}
					return nil
				}
				set := newWorkerTestSet(t, api)
				_, err := set.Probe(t.Context(), KindBackend)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertOperationError(
				t,
				test.run(),
				test.operation,
				injectedErr,
			)
		})
	}
}
