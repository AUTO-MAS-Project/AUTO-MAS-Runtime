//go:build windows

package lock

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type acquireCallResult struct {
	result AcquisitionResult
	err    error
}

type frozenWait struct {
	name      string
	result    uint32
	err       error
	owned     bool
	recovered bool
}

type releaseFailure struct {
	name   string
	handle windows.Handle
}

func newAcquireWorkerState(api windowsAPI) *workerState {
	return &workerState{
		api: api,
		backend: mutexState{
			kind:   KindBackend,
			name:   "backend-test",
			handle: testBackendHandle,
		},
		mutation: mutexState{
			kind:   KindMutation,
			name:   "mutation-test",
			handle: testMutationHandle,
		},
	}
}

func countHandleCalls(
	api *testWindowsAPI,
	operation string,
	handle windows.Handle,
) int {
	count := 0
	for _, call := range api.callsFor(operation) {
		if call.Handle == handle {
			count++
		}
	}
	return count
}

func assertErrorCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	var coded interface {
		Code() protocol.Code
	}
	if !errors.As(err, &coded) {
		t.Fatalf("error = %v, want error with Code()", err)
	}
	if got := coded.Code(); got != want {
		t.Fatalf("Code() = %q, want %q", got, want)
	}
}

func acquireWaitCases(injectedErr error) []frozenWait {
	return []frozenWait{
		{
			name:   "success",
			result: waitResultObject0,
			owned:  true,
		},
		{
			name:      "abandoned",
			result:    waitResultAbandoned,
			owned:     true,
			recovered: true,
		},
		{
			name:   "timeout",
			result: waitResultTimeout,
		},
		{
			name:   "failed",
			result: waitResultFailed,
			err:    injectedErr,
		},
		{
			name:   "unexpected",
			result: 0x42,
		},
	}
}

func TestSet_AcquireBackendMapsConflicts(t *testing.T) {
	tests := []struct {
		name               string
		wait               func(apiCall) (uint32, error)
		wantCode           protocol.Code
		wantPeerWaits      int
		wantPrimaryRelease int
	}{
		{
			name: "backend held",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testBackendHandle {
					return waitResultTimeout, nil
				}
				return waitResultObject0, nil
			},
			wantCode: protocol.CodeBackendAlreadyRunning,
		},
		{
			name: "mutation held",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testMutationHandle {
					return waitResultTimeout, nil
				}
				return waitResultObject0, nil
			},
			wantCode:           protocol.CodeMutationInProgress,
			wantPeerWaits:      1,
			wantPrimaryRelease: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = test.wait
			set := newWorkerTestSet(t, api)
			result, err := set.AcquireBackend(t.Context())
			assertErrorCode(t, err, test.wantCode)
			if result.Lease() != nil {
				t.Fatal("Lease() != nil, want conflict")
			}
			if got := countHandleCalls(
				api,
				"wait",
				testMutationHandle,
			); got != test.wantPeerWaits {
				t.Fatalf("peer Wait calls = %d, want %d", got, test.wantPeerWaits)
			}
			if got := countHandleCalls(
				api,
				"release",
				testBackendHandle,
			); got != test.wantPrimaryRelease {
				t.Fatalf(
					"primary Release calls = %d, want %d",
					got,
					test.wantPrimaryRelease,
				)
			}
		})
	}
}

func TestSet_AcquireMutationMapsConflicts(t *testing.T) {
	tests := []struct {
		name               string
		wait               func(apiCall) (uint32, error)
		wantCode           protocol.Code
		wantPeerWaits      int
		wantPrimaryRelease int
	}{
		{
			name: "mutation held",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testMutationHandle {
					return waitResultTimeout, nil
				}
				return waitResultObject0, nil
			},
			wantCode: protocol.CodeMutationInProgress,
		},
		{
			name: "backend held",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testBackendHandle {
					return waitResultTimeout, nil
				}
				return waitResultObject0, nil
			},
			wantCode:           protocol.CodeBackendStillRunning,
			wantPeerWaits:      1,
			wantPrimaryRelease: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = test.wait
			set := newWorkerTestSet(t, api)
			result, err := set.AcquireMutation(t.Context())
			assertErrorCode(t, err, test.wantCode)
			if result.Lease() != nil {
				t.Fatal("Lease() != nil, want conflict")
			}
			if got := countHandleCalls(
				api,
				"wait",
				testBackendHandle,
			); got != test.wantPeerWaits {
				t.Fatalf("peer Wait calls = %d, want %d", got, test.wantPeerWaits)
			}
			if got := countHandleCalls(
				api,
				"release",
				testMutationHandle,
			); got != test.wantPrimaryRelease {
				t.Fatalf(
					"primary Release calls = %d, want %d",
					got,
					test.wantPrimaryRelease,
				)
			}
		})
	}
}

func TestSet_OperationsStayOnPinnedThread(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	if err := result.Lease().Close(); err != nil {
		t.Fatalf("Lease.Close() error = %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}

	duplicate := api.callsFor("duplicate-thread")
	if len(duplicate) != 1 {
		t.Fatalf("DuplicateHandle calls = %d, want 1", len(duplicate))
	}
	pinnedID := duplicate[0].ThreadID
	for _, operation := range []string{"wait", "release", "close"} {
		for _, call := range api.callsFor(operation) {
			if operation == "close" && call.Handle == testThreadHandle {
				continue
			}
			if call.ThreadID != pinnedID {
				t.Fatalf(
					"%s handle %#x thread = %d, want %d",
					operation,
					call.Handle,
					call.ThreadID,
					pinnedID,
				)
			}
		}
	}
}

func TestSet_AcquireChecksContextBeforeQueueWaitAndReply(t *testing.T) {
	t.Run("before queue", func(t *testing.T) {
		api := newTestWindowsAPI()
		set := newWorkerTestSet(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result, err := set.AcquireBackend(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AcquireBackend() error = %v, want context.Canceled", err)
		}
		if result.Lease() != nil || api.count("wait") != 0 {
			t.Fatalf(
				"Lease/Wait calls = %v/%d, want nil/0",
				result.Lease(),
				api.count("wait"),
			)
		}
	})

	t.Run("before worker Wait", func(t *testing.T) {
		api := newTestWindowsAPI()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		state := newAcquireWorkerState(api)
		response := state.acquire(workerRequest{
			ctx:  ctx,
			kind: KindBackend,
		})
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("acquire() error = %v, want context.Canceled", response.err)
		}
		if api.count("wait") != 0 {
			t.Fatalf("Wait calls = %d, want 0", api.count("wait"))
		}
	})

	for _, failure := range []releaseFailure{
		{name: "release success"},
		{name: "peer Release failure", handle: testMutationHandle},
		{name: "primary Release failure", handle: testBackendHandle},
	} {
		t.Run("before reply/"+failure.name, func(t *testing.T) {
			runAcquireReplyCancellation(t, failure.handle)
		})
	}

	t.Run("before reply/both Release failures", func(t *testing.T) {
		runAcquireReplyDualReleaseFailure(t)
	})
}

func runAcquireReplyCancellation(
	t *testing.T,
	failingHandle windows.Handle,
) {
	t.Helper()
	injectedErr := errors.New("injected release failure")
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
			call.Handle == testMutationHandle {
			once.Do(func() { close(releaseReturned) })
			<-allowReply
		}
	}
	set := newWorkerTestSet(t, api)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultCh := make(chan acquireCallResult, 1)
	go func() {
		result, err := set.AcquireBackend(ctx)
		resultCh <- acquireCallResult{result: result, err: err}
	}()
	waitSignal(t, releaseReturned, "peer Release return")
	assertPending(t, resultCh, "AcquireBackend")
	cancel()
	close(allowReply)
	call := waitValue(t, resultCh, "AcquireBackend reply cancellation")
	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("AcquireBackend() error = %v, want context.Canceled", call.err)
	}
	if failingHandle != 0 && !errors.Is(call.err, injectedErr) {
		t.Fatalf("AcquireBackend() error = %v, want release failure", call.err)
	}
	if call.result.Lease() != nil {
		t.Fatal("Lease() != nil, want cancellation")
	}
	if got := countHandleCalls(
		api,
		"release",
		testMutationHandle,
	); got != 1 {
		t.Fatalf("peer Release calls = %d, want 1", got)
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != 1 {
		t.Fatalf("primary Release calls = %d, want 1", got)
	}
}

func runAcquireReplyDualReleaseFailure(t *testing.T) {
	t.Helper()
	peerErr := errors.New("injected peer release failure")
	primaryErr := errors.New("injected primary release failure")
	api := newTestWindowsAPI()
	api.releaseErr = func(call apiCall) error {
		switch call.Handle {
		case testMutationHandle:
			return peerErr
		case testBackendHandle:
			return primaryErr
		default:
			return nil
		}
	}
	releaseReturned := make(chan struct{})
	allowReply := make(chan struct{})
	var once sync.Once
	api.afterCall = func(call apiCall) {
		if call.Operation == "release" &&
			call.Handle == testMutationHandle {
			once.Do(func() { close(releaseReturned) })
			<-allowReply
		}
	}
	set := newWorkerTestSet(t, api)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultCh := make(chan acquireCallResult, 1)
	go func() {
		result, err := set.AcquireBackend(ctx)
		resultCh <- acquireCallResult{result: result, err: err}
	}()
	waitSignal(t, releaseReturned, "peer Release return")
	cancel()
	close(allowReply)
	call := waitValue(t, resultCh, "dual release failure Acquire")
	if !errors.Is(call.err, context.Canceled) ||
		!errors.Is(call.err, peerErr) ||
		!errors.Is(call.err, primaryErr) {
		t.Fatalf(
			"AcquireBackend() error = %v, want cancellation and both release failures",
			call.err,
		)
	}
	if call.result.Lease() != nil {
		t.Fatal("Lease() != nil, want cancellation")
	}

	_, poisonedErr := set.AcquireMutation(t.Context())
	if !errors.Is(poisonedErr, ErrPoisoned) ||
		!errors.Is(poisonedErr, peerErr) ||
		errors.Is(poisonedErr, primaryErr) {
		t.Fatalf(
			"sticky poison error = %v, want only first peer release failure",
			poisonedErr,
		)
	}
}

func TestSet_AcquireChecksContextAfterEveryWaitResult(t *testing.T) {
	injectedWaitErr := errors.New("injected wait failure")
	for _, wait := range acquireWaitCases(injectedWaitErr) {
		failures := []releaseFailure{{name: "Release success"}}
		if wait.owned {
			failures = append(failures, releaseFailure{
				name:   "primary Release failure",
				handle: testBackendHandle,
			})
		}
		for _, failure := range failures {
			t.Run(
				"primary/"+wait.name+"/"+failure.name,
				func(t *testing.T) {
					runCanceledAcquireWait(
						t,
						"primary",
						wait,
						failure.handle,
					)
				},
			)
		}
	}

	for _, wait := range acquireWaitCases(injectedWaitErr) {
		failures := []releaseFailure{
			{name: "Release success"},
			{
				name:   "primary Release failure",
				handle: testBackendHandle,
			},
		}
		if wait.owned {
			failures = append(failures, releaseFailure{
				name:   "peer Release failure",
				handle: testMutationHandle,
			})
		}
		for _, failure := range failures {
			t.Run(
				"peer/"+wait.name+"/"+failure.name,
				func(t *testing.T) {
					runCanceledAcquireWait(
						t,
						"peer",
						wait,
						failure.handle,
					)
				},
			)
		}
	}
}

func runCanceledAcquireWait(
	t *testing.T,
	location string,
	wait frozenWait,
	failingHandle windows.Handle,
) {
	t.Helper()
	injectedReleaseErr := errors.New("injected release failure")
	api := newTestWindowsAPI()
	waitReturned := make(chan struct{})
	allowWorker := make(chan struct{})
	var once sync.Once
	target := testBackendHandle
	if location == "peer" {
		target = testMutationHandle
	}
	api.waitResult = func(call apiCall) (uint32, error) {
		if call.Handle != target {
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
	resultCh := make(chan acquireCallResult, 1)
	go func() {
		result, err := set.AcquireBackend(ctx)
		resultCh <- acquireCallResult{result: result, err: err}
	}()
	waitSignal(t, waitReturned, location+" Wait return")
	cancel()
	close(allowWorker)
	call := waitValue(t, resultCh, location+" canceled Acquire")

	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("AcquireBackend() error = %v, want context.Canceled", call.err)
	}
	if failingHandle != 0 && !errors.Is(call.err, injectedReleaseErr) {
		t.Fatalf("AcquireBackend() error = %v, want release failure", call.err)
	}
	if call.result.Lease() != nil {
		t.Fatal("Lease() != nil, want cancellation")
	}
	if got := countHandleCalls(
		api,
		"wait",
		testBackendHandle,
	); got != 1 {
		t.Fatalf("primary Wait calls = %d, want 1", got)
	}

	wantPrimaryRelease := 0
	wantPeerWait := 0
	wantPeerRelease := 0
	if location == "primary" {
		if wait.owned {
			wantPrimaryRelease = 1
		}
		if call.result.Recovered() != wait.recovered {
			t.Fatalf(
				"Recovered() = %t, want %t",
				call.result.Recovered(),
				wait.recovered,
			)
		}
	} else {
		wantPrimaryRelease = 1
		wantPeerWait = 1
		if wait.owned {
			wantPeerRelease = 1
		}
		if call.result.PeerProbe().Recovered != wait.recovered {
			t.Fatalf(
				"PeerProbe().Recovered = %t, want %t",
				call.result.PeerProbe().Recovered,
				wait.recovered,
			)
		}
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != wantPrimaryRelease {
		t.Fatalf(
			"primary Release calls = %d, want %d",
			got,
			wantPrimaryRelease,
		)
	}
	if got := countHandleCalls(
		api,
		"wait",
		testMutationHandle,
	); got != wantPeerWait {
		t.Fatalf("peer Wait calls = %d, want %d", got, wantPeerWait)
	}
	if got := countHandleCalls(
		api,
		"release",
		testMutationHandle,
	); got != wantPeerRelease {
		t.Fatalf(
			"peer Release calls = %d, want %d",
			got,
			wantPeerRelease,
		)
	}
}

func TestSet_AcquireFailureReleasesPrimary(t *testing.T) {
	injectedErr := errors.New("injected peer wait failure")
	tests := []struct {
		name string
		wait func(apiCall) (uint32, error)
	}{
		{
			name: "peer conflict",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testMutationHandle {
					return waitResultTimeout, nil
				}
				return waitResultObject0, nil
			},
		},
		{
			name: "peer failure",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testMutationHandle {
					return waitResultFailed, injectedErr
				}
				return waitResultObject0, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = test.wait
			set := newWorkerTestSet(t, api)
			result, err := set.AcquireBackend(t.Context())
			if err == nil || result.Lease() != nil {
				t.Fatalf(
					"AcquireBackend() = (%v, %v), want failure",
					result.Lease(),
					err,
				)
			}
			if got := countHandleCalls(
				api,
				"release",
				testBackendHandle,
			); got != 1 {
				t.Fatalf("primary Release calls = %d, want 1", got)
			}
			api.waitResult = nil
			retry, err := set.AcquireBackend(t.Context())
			if err != nil {
				t.Fatalf("retry AcquireBackend() error = %v", err)
			}
			if err := retry.Lease().Close(); err != nil {
				t.Fatalf("retry Lease.Close() error = %v", err)
			}
		})
	}

	t.Run("reply cancellation", func(t *testing.T) {
		api := newTestWindowsAPI()
		releaseReturned := make(chan struct{})
		allowReply := make(chan struct{})
		var once sync.Once
		api.afterCall = func(call apiCall) {
			if call.Operation == "release" &&
				call.Handle == testMutationHandle {
				once.Do(func() { close(releaseReturned) })
				<-allowReply
			}
		}
		set := newWorkerTestSet(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan acquireCallResult, 1)
		go func() {
			result, err := set.AcquireBackend(ctx)
			resultCh <- acquireCallResult{result: result, err: err}
		}()
		waitSignal(t, releaseReturned, "peer Release")
		cancel()
		close(allowReply)
		call := waitValue(t, resultCh, "canceled Acquire retry setup")
		if !errors.Is(call.err, context.Canceled) {
			t.Fatalf(
				"AcquireBackend() error = %v, want context.Canceled",
				call.err,
			)
		}
		if got := countHandleCalls(
			api,
			"release",
			testBackendHandle,
		); got != 1 {
			t.Fatalf("primary Release calls = %d, want 1", got)
		}
		api.afterCall = nil
		retry, err := set.AcquireBackend(t.Context())
		if err != nil {
			t.Fatalf("retry AcquireBackend() error = %v", err)
		}
		if err := retry.Lease().Close(); err != nil {
			t.Fatalf("retry Lease.Close() error = %v", err)
		}
	})
}

func TestSet_AbandonedMutexRecordsRecovery(t *testing.T) {
	t.Run("primary success", func(t *testing.T) {
		api := newTestWindowsAPI()
		api.waitResult = func(call apiCall) (uint32, error) {
			if call.Handle == testBackendHandle {
				return waitResultAbandoned, nil
			}
			return waitResultObject0, nil
		}
		set := newWorkerTestSet(t, api)
		result, err := set.AcquireBackend(t.Context())
		if err != nil {
			t.Fatalf("AcquireBackend() error = %v", err)
		}
		if !result.Recovered() || result.Lease() == nil {
			t.Fatalf(
				"Recovered/Lease = %t/%v, want true/non-nil",
				result.Recovered(),
				result.Lease(),
			)
		}
		if err := result.Lease().Close(); err != nil {
			t.Fatalf("Lease.Close() error = %v", err)
		}
	})

	t.Run("peer success", func(t *testing.T) {
		api := newTestWindowsAPI()
		api.waitResult = func(call apiCall) (uint32, error) {
			if call.Handle == testMutationHandle {
				return waitResultAbandoned, nil
			}
			return waitResultObject0, nil
		}
		set := newWorkerTestSet(t, api)
		result, err := set.AcquireBackend(t.Context())
		if err != nil {
			t.Fatalf("AcquireBackend() error = %v", err)
		}
		if !result.PeerProbe().Recovered || result.Lease() == nil {
			t.Fatalf(
				"Peer recovered/Lease = %t/%v, want true/non-nil",
				result.PeerProbe().Recovered,
				result.Lease(),
			)
		}
		if err := result.Lease().Close(); err != nil {
			t.Fatalf("Lease.Close() error = %v", err)
		}
	})

	injectedWaitErr := errors.New("injected peer failure")
	for _, test := range []struct {
		name string
		wait func(apiCall) (uint32, error)
	}{
		{
			name: "peer conflict",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testBackendHandle {
					return waitResultAbandoned, nil
				}
				return waitResultTimeout, nil
			},
		},
		{
			name: "peer failure",
			wait: func(call apiCall) (uint32, error) {
				if call.Handle == testBackendHandle {
					return waitResultAbandoned, nil
				}
				return waitResultFailed, injectedWaitErr
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			api.waitResult = test.wait
			set := newWorkerTestSet(t, api)
			result, err := set.AcquireBackend(t.Context())
			if err == nil || result.Lease() != nil || !result.Recovered() {
				t.Fatalf(
					"AcquireBackend() = (recovered=%t, lease=%v, err=%v)",
					result.Recovered(),
					result.Lease(),
					err,
				)
			}
		})
	}

	t.Run("reply cancellation", func(t *testing.T) {
		api := newTestWindowsAPI()
		api.waitResult = func(call apiCall) (uint32, error) {
			if call.Handle == testBackendHandle {
				return waitResultAbandoned, nil
			}
			return waitResultObject0, nil
		}
		releaseReturned := make(chan struct{})
		allowReply := make(chan struct{})
		var once sync.Once
		api.afterCall = func(call apiCall) {
			if call.Operation == "release" &&
				call.Handle == testMutationHandle {
				once.Do(func() { close(releaseReturned) })
				<-allowReply
			}
		}
		set := newWorkerTestSet(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan acquireCallResult, 1)
		go func() {
			result, err := set.AcquireBackend(ctx)
			resultCh <- acquireCallResult{result: result, err: err}
		}()
		waitSignal(t, releaseReturned, "peer Release")
		cancel()
		close(allowReply)
		call := waitValue(t, resultCh, "abandoned Acquire cancellation")
		if !errors.Is(call.err, context.Canceled) ||
			!call.result.Recovered() ||
			call.result.Lease() != nil {
			t.Fatalf(
				"AcquireBackend() = (recovered=%t, lease=%v, err=%v)",
				call.result.Recovered(),
				call.result.Lease(),
				call.err,
			)
		}
	})

	t.Run("primary cleanup failure", func(t *testing.T) {
		injectedReleaseErr := errors.New("injected primary release failure")
		api := newTestWindowsAPI()
		api.waitResult = func(call apiCall) (uint32, error) {
			if call.Handle == testBackendHandle {
				return waitResultAbandoned, nil
			}
			return waitResultTimeout, nil
		}
		api.releaseErr = func(call apiCall) error {
			if call.Handle == testBackendHandle {
				return injectedReleaseErr
			}
			return nil
		}
		set := newWorkerTestSet(t, api)
		result, err := set.AcquireBackend(t.Context())
		if !errors.Is(err, injectedReleaseErr) ||
			!result.Recovered() ||
			result.Lease() != nil {
			t.Fatalf(
				"AcquireBackend() = (recovered=%t, lease=%v, err=%v)",
				result.Recovered(),
				result.Lease(),
				err,
			)
		}
	})
}

func TestSet_RejectsSecondLiveLeaseBeforeWait(t *testing.T) {
	tests := []struct {
		name     string
		active   Kind
		request  Kind
		wantCode protocol.Code
	}{
		{
			name:     "backend then backend",
			active:   KindBackend,
			request:  KindBackend,
			wantCode: protocol.CodeBackendAlreadyRunning,
		},
		{
			name:     "backend then mutation",
			active:   KindBackend,
			request:  KindMutation,
			wantCode: protocol.CodeBackendStillRunning,
		},
		{
			name:     "mutation then backend",
			active:   KindMutation,
			request:  KindBackend,
			wantCode: protocol.CodeMutationInProgress,
		},
		{
			name:     "mutation then mutation",
			active:   KindMutation,
			request:  KindMutation,
			wantCode: protocol.CodeMutationInProgress,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			set := newWorkerTestSet(t, api)
			var active AcquisitionResult
			var err error
			if test.active == KindBackend {
				active, err = set.AcquireBackend(t.Context())
			} else {
				active, err = set.AcquireMutation(t.Context())
			}
			if err != nil {
				t.Fatalf("initial Acquire() error = %v", err)
			}
			before := api.count("wait")
			var second AcquisitionResult
			if test.request == KindBackend {
				second, err = set.AcquireBackend(t.Context())
			} else {
				second, err = set.AcquireMutation(t.Context())
			}
			assertErrorCode(t, err, test.wantCode)
			if second.Lease() != nil {
				t.Fatal("second Lease() != nil, want conflict")
			}
			if got := api.count("wait"); got != before {
				t.Fatalf("Wait calls = %d, want unchanged %d", got, before)
			}
			if err := active.Lease().Close(); err != nil {
				t.Fatalf("active Lease.Close() error = %v", err)
			}
		})
	}
}

func TestLease_CloseIsIdempotentAndGenerationSafe(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	first, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("first AcquireBackend() error = %v", err)
	}
	firstLease := first.Lease()
	firstGeneration := firstLease.generation
	if err := firstLease.Close(); err != nil {
		t.Fatalf("first Lease.Close() error = %v", err)
	}
	if err := firstLease.Close(); err != nil {
		t.Fatalf("repeated first Lease.Close() error = %v", err)
	}
	second, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("second AcquireBackend() error = %v", err)
	}
	before := countHandleCalls(api, "release", testBackendHandle)
	if err := set.release(KindBackend, firstGeneration); err != nil {
		t.Fatalf("stale generation release error = %v", err)
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != before {
		t.Fatalf("backend Release calls = %d, want unchanged %d", got, before)
	}
	if err := second.Lease().Close(); err != nil {
		t.Fatalf("second Lease.Close() error = %v", err)
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != before+1 {
		t.Fatalf("backend Release calls = %d, want %d", got, before+1)
	}
}

func TestLease_CloseAfterSetCloseReturnsNil(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}
	before := api.count("release")
	if err := result.Lease().Close(); err != nil {
		t.Fatalf("old Lease.Close() error = %v", err)
	}
	if got := api.count("release"); got != before {
		t.Fatalf("Release calls = %d, want unchanged %d", got, before)
	}
}

func TestSet_CloseIsIdempotent(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	const callers = 32
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			ready.Done()
			<-start
			errs <- set.Close()
		}()
	}
	waitGroup(t, &ready, "active Lease Close callers ready")
	close(start)
	for i := 0; i < callers; i++ {
		if err := waitValue(t, errs, "active Lease concurrent Set.Close"); err != nil {
			t.Fatalf("Set.Close() error = %v", err)
		}
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != 1 {
		t.Fatalf("backend Release calls = %d, want 1", got)
	}
	if _, err := set.AcquireBackend(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("AcquireBackend() error = %v, want ErrClosed", err)
	}
	if err := result.Lease().Close(); err != nil {
		t.Fatalf("old Lease.Close() error = %v", err)
	}
}

func TestSet_CloseWithActiveLeaseReleasesBeforeUnlock(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}
	releases := api.callsFor("release")
	if len(releases) != 2 ||
		releases[0].Handle != testMutationHandle ||
		releases[1].Handle != testBackendHandle {
		t.Fatalf(
			"Release handles = %#v, want peer then active backend",
			releases,
		)
	}
	if api.count("wait") != 2 {
		t.Fatalf("Wait calls = %d, want 2", api.count("wait"))
	}
	if err := result.Lease().Close(); err != nil {
		t.Fatalf("old Lease.Close() error = %v", err)
	}
}

func TestSet_CloseAndLeaseCloseLinearizeByAdmissionOrder(t *testing.T) {
	t.Run("release first", func(t *testing.T) {
		harness := newAdmissionHarness()
		go harness.run()
		leaseResult := make(chan error, 1)
		go func() { leaseResult <- harness.lease.Close() }()
		waitSignal(t, harness.releaseAccepted, "release admission")

		closeResult := make(chan error, 1)
		go func() { closeResult <- harness.set.Close() }()
		waitForSetClosing(t, harness.set)
		assertPending(t, closeResult, "Set.Close")
		close(harness.allowRelease)
		waitSignal(t, harness.closeAccepted, "Close admission")
		close(harness.allowClose)
		if err := waitValue(t, leaseResult, "release-first Lease.Close"); err != nil {
			t.Fatalf("Lease.Close() error = %v", err)
		}
		if err := waitValue(t, closeResult, "release-first Set.Close"); err != nil {
			t.Fatalf("Set.Close() error = %v", err)
		}
		if got := countHandleCalls(
			harness.api,
			"release",
			testBackendHandle,
		); got != 1 {
			t.Fatalf("backend Release calls = %d, want 1", got)
		}
		assertRequestChannelOpen(t, harness.set.requests)
	})

	t.Run("close first", func(t *testing.T) {
		harness := newAdmissionHarness()
		go harness.run()
		closeResult := make(chan error, 1)
		go func() { closeResult <- harness.set.Close() }()
		waitSignal(t, harness.closeAccepted, "Close admission")

		leaseResult := make(chan error, 1)
		go func() { leaseResult <- harness.lease.Close() }()
		assertPending(t, leaseResult, "Lease.Close")
		close(harness.allowClose)
		if err := waitValue(t, closeResult, "close-first Set.Close"); err != nil {
			t.Fatalf("Set.Close() error = %v", err)
		}
		if err := waitValue(t, leaseResult, "close-first Lease.Close"); err != nil {
			t.Fatalf("Lease.Close() error = %v", err)
		}
		if got := countHandleCalls(
			harness.api,
			"release",
			testBackendHandle,
		); got != 1 {
			t.Fatalf("backend Release calls = %d, want 1", got)
		}
		assertRequestChannelOpen(t, harness.set.requests)
	})
}

type admissionHarness struct {
	api             *testWindowsAPI
	set             *Set
	state           *workerState
	lease           *Lease
	releaseAccepted chan struct{}
	allowRelease    chan struct{}
	closeAccepted   chan struct{}
	allowClose      chan struct{}
}

func newAdmissionHarness() *admissionHarness {
	api := newTestWindowsAPI()
	requests := make(chan workerRequest)
	set := &Set{
		requests:  requests,
		closeDone: make(chan struct{}),
		thread:    testThreadHandle,
		api:       api,
	}
	state := newAcquireWorkerState(api)
	state.activeKind = KindBackend
	state.activeGeneration = 1
	state.backend.maybeOwned = true
	return &admissionHarness{
		api:   api,
		set:   set,
		state: state,
		lease: &Lease{
			set:        set,
			kind:       KindBackend,
			name:       state.backend.name,
			generation: 1,
		},
		releaseAccepted: make(chan struct{}),
		allowRelease:    make(chan struct{}),
		closeAccepted:   make(chan struct{}),
		allowClose:      make(chan struct{}),
	}
}

func (h *admissionHarness) run() {
	for {
		request := <-h.set.requests
		switch request.operation {
		case requestRelease:
			close(h.releaseAccepted)
			<-h.allowRelease
			request.response <- h.state.release(request)
		case requestClose:
			close(h.closeAccepted)
			<-h.allowClose
			closeErr := h.state.releaseSlotForClose(&h.state.backend)
			request.response <- workerResponse{
				exit: workerExit{},
				err:  closeErr,
			}
			return
		}
	}
}

func waitForSetClosing(t *testing.T, set *Set) {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	for {
		set.stateMu.Lock()
		closing := set.closing
		set.stateMu.Unlock()
		if closing {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("timed out waiting for closing state")
		default:
			runtime.Gosched()
		}
	}
}

func assertRequestChannelOpen(t *testing.T, requests chan workerRequest) {
	t.Helper()
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		select {
		case requests <- workerRequest{}:
			t.Fatal("unexpected request receiver after Close")
		default:
		}
	}()
	if panicked {
		t.Fatal("request channel was closed")
	}
}

func TestSet_ReleaseFailurePoisonsSet(t *testing.T) {
	injectedErr := errors.New("injected Lease release failure")
	api := newTestWindowsAPI()
	api.releaseErr = func(call apiCall) error {
		if call.Handle == testBackendHandle {
			return injectedErr
		}
		return nil
	}
	set := newWorkerTestSet(t, api)
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	firstErr := result.Lease().Close()
	if !errors.Is(firstErr, injectedErr) ||
		!errors.Is(firstErr, ErrPoisoned) {
		t.Fatalf("Lease.Close() error = %v, want poison and cause", firstErr)
	}
	if repeated := result.Lease().Close(); repeated != firstErr {
		t.Fatalf("repeated Lease.Close() error = %v, want cached %v", repeated, firstErr)
	}
	beforeWait := api.count("wait")
	if _, err := set.AcquireMutation(t.Context()); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("AcquireMutation() error = %v, want ErrPoisoned", err)
	}
	if got := api.count("wait"); got != beforeWait {
		t.Fatalf("Wait calls = %d, want unchanged %d", got, beforeWait)
	}
	closeErr := set.Close()
	if !errors.Is(closeErr, injectedErr) {
		t.Fatalf("Set.Close() error = %v, want injected cause", closeErr)
	}
	if got := countHandleCalls(
		api,
		"release",
		testBackendHandle,
	); got != 2 {
		t.Fatalf("backend Release calls = %d, want Lease+Close retries", got)
	}
	if repeated := set.Close(); repeated != closeErr {
		t.Fatalf("repeated Set.Close() error = %v, want cached %v", repeated, closeErr)
	}
}

func testAcquireOperationErrors(t *testing.T) {
	t.Helper()
	injectedErr := errors.New("injected acquire failure")
	tests := []struct {
		name      string
		operation string
		run       func() error
	}{
		{
			name:      "wait-primary",
			operation: "wait-primary",
			run: func() error {
				api := newTestWindowsAPI()
				api.waitResult = func(call apiCall) (uint32, error) {
					if call.Handle == testBackendHandle {
						return waitResultFailed, injectedErr
					}
					return waitResultObject0, nil
				}
				set := newWorkerTestSet(t, api)
				_, err := set.AcquireBackend(t.Context())
				return err
			},
		},
		{
			name:      "wait-peer",
			operation: "wait-peer",
			run: func() error {
				api := newTestWindowsAPI()
				api.waitResult = func(call apiCall) (uint32, error) {
					if call.Handle == testMutationHandle {
						return waitResultFailed, injectedErr
					}
					return waitResultObject0, nil
				}
				set := newWorkerTestSet(t, api)
				_, err := set.AcquireBackend(t.Context())
				return err
			},
		},
		{
			name:      "release-primary",
			operation: "release-primary",
			run: func() error {
				api := newTestWindowsAPI()
				api.waitResult = func(call apiCall) (uint32, error) {
					if call.Handle == testMutationHandle {
						return waitResultTimeout, nil
					}
					return waitResultObject0, nil
				}
				api.releaseErr = func(call apiCall) error {
					if call.Handle == testBackendHandle {
						return injectedErr
					}
					return nil
				}
				set := newWorkerTestSet(t, api)
				_, err := set.AcquireBackend(t.Context())
				return err
			},
		},
		{
			name:      "release-peer",
			operation: "release-peer",
			run: func() error {
				api := newTestWindowsAPI()
				api.releaseErr = func(call apiCall) error {
					if call.Handle == testMutationHandle {
						return injectedErr
					}
					return nil
				}
				set := newWorkerTestSet(t, api)
				_, err := set.AcquireBackend(t.Context())
				return err
			},
		},
		{
			name:      "release-lease",
			operation: "release-lease",
			run: func() error {
				api := newTestWindowsAPI()
				api.releaseErr = func(call apiCall) error {
					if call.Handle == testBackendHandle {
						return injectedErr
					}
					return nil
				}
				set := newWorkerTestSet(t, api)
				result, err := set.AcquireBackend(t.Context())
				if err != nil {
					return err
				}
				return result.Lease().Close()
			},
		},
		{
			name:      "release-on-close",
			operation: "release-on-close",
			run: func() error {
				api := newTestWindowsAPI()
				api.releaseErr = func(call apiCall) error {
					if call.Handle == testBackendHandle {
						return injectedErr
					}
					return nil
				}
				set := newWorkerTestSet(t, api)
				if _, err := set.AcquireBackend(t.Context()); err != nil {
					return err
				}
				return set.Close()
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
