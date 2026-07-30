//go:build windows

package lock

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

const testWaitLimit = 5 * time.Second

func workerTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(
		`C:\AUTO-MAS-test\app`,
		`C:\AUTO-MAS-test`,
	)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func newWorkerTestSet(
	t *testing.T,
	api *testWindowsAPI,
) *Set {
	t.Helper()
	set, err := newSet(t.Context(), workerTestLayout(t), api)
	if err != nil {
		t.Fatalf("newSet() error = %v", err)
	}
	t.Cleanup(func() {
		// 测试主体断言预期错误；cleanup 只在有限期限内保证幂等收口。
		done := make(chan struct{})
		go func() {
			_ = set.Close()
			close(done)
		}()
		waitSignal(t, done, "Set.Close cleanup")
	})
	return set
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitValue[T any](t *testing.T, values <-chan T, name string) T {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	select {
	case value := <-values:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func waitGroup(t *testing.T, group *sync.WaitGroup, name string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	waitSignal(t, done, name)
}

func assertPending[T any](t *testing.T, result <-chan T, name string) {
	t.Helper()
	select {
	case <-result:
		t.Fatalf("%s completed before barrier", name)
	default:
	}
}

func TestSet_StartupUsesExactWin32Arguments(t *testing.T) {
	api := newTestWindowsAPI()
	_ = newWorkerTestSet(t, api)

	if got := api.count("current-process"); got != 1 {
		t.Fatalf("currentProcess calls = %d, want 1", got)
	}
	if got := api.count("current-thread"); got != 1 {
		t.Fatalf("currentThread calls = %d, want 1", got)
	}
	duplicateCalls := api.callsFor("duplicate-thread")
	if len(duplicateCalls) != 1 {
		t.Fatalf(
			"DuplicateHandle calls = %d, want 1",
			len(duplicateCalls),
		)
	}
	duplicate := duplicateCalls[0]
	if duplicate.SourceProcess != testProcessHandle ||
		duplicate.SourceHandle != testThreadPseudo ||
		duplicate.TargetProcess != testProcessHandle {
		t.Fatalf(
			"DuplicateHandle source process/handle/target process = %#x/%#x/%#x, want %#x/%#x/%#x",
			duplicate.SourceProcess,
			duplicate.SourceHandle,
			duplicate.TargetProcess,
			testProcessHandle,
			testThreadPseudo,
			testProcessHandle,
		)
	}
	if duplicate.DesiredAccess != windows.SYNCHRONIZE ||
		duplicate.InheritHandle ||
		duplicate.Options != 0 {
		t.Fatalf(
			"DuplicateHandle access/inherit/options = %#x/%t/%#x, want %#x/false/0",
			duplicate.DesiredAccess,
			duplicate.InheritHandle,
			duplicate.Options,
			uint32(windows.SYNCHRONIZE),
		)
	}

	mutexCalls := api.callsFor("create-mutex")
	wantNames := []string{
		`Local\AUTO-MAS-Runtime-backend-83839c3a1d1a406c38e8b2a0d187f211`,
		`Local\AUTO-MAS-Runtime-mutation-83839c3a1d1a406c38e8b2a0d187f211`,
	}
	if len(mutexCalls) != len(wantNames) {
		t.Fatalf(
			"CreateMutex calls = %d, want %d",
			len(mutexCalls),
			len(wantNames),
		)
	}
	for index, call := range mutexCalls {
		if !call.SecurityWasNil || call.InitialOwner {
			t.Fatalf(
				"CreateMutex[%d] security nil/initial owner = %t/%t, want true/false",
				index,
				call.SecurityWasNil,
				call.InitialOwner,
			)
		}
		if call.Name != wantNames[index] {
			t.Fatalf(
				"CreateMutex[%d] name = %q, want %q",
				index,
				call.Name,
				wantNames[index],
			)
		}
	}
}

func TestNewSet_ChecksContextAtAllBoundaries(t *testing.T) {
	t.Run("before root I/O", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		api := newTestWindowsAPI()
		_, err := newSet(ctx, workerTestLayout(t), api)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("newSet() error = %v, want context.Canceled", err)
		}
		if api.count("create-file") != 0 {
			t.Fatalf(
				"CreateFile calls = %d, want 0",
				api.count("create-file"),
			)
		}
	})

	t.Run("before first CreateMutex", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		api := newTestWindowsAPI()
		api.afterCall = func(call apiCall) {
			if call.Operation == "duplicate-thread" {
				cancel()
			}
		}
		_, err := newSet(ctx, workerTestLayout(t), api)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("newSet() error = %v, want context.Canceled", err)
		}
		if api.count("create-mutex") != 0 {
			t.Fatalf(
				"CreateMutex calls = %d, want 0",
				api.count("create-mutex"),
			)
		}
		if api.count("close") != 2 {
			t.Fatalf(
				"CloseHandle calls = %d, want root+thread",
				api.count("close"),
			)
		}
	})

	t.Run("after both Mutex handles", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		api := newTestWindowsAPI()
		api.afterCall = func(call apiCall) {
			if call.Operation == "create-mutex" && call.Index == 2 {
				cancel()
			}
		}
		_, err := newSet(ctx, workerTestLayout(t), api)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("newSet() error = %v, want context.Canceled", err)
		}
		if api.count("create-mutex") != 2 {
			t.Fatalf(
				"CreateMutex calls = %d, want 2",
				api.count("create-mutex"),
			)
		}
		if api.count("close") != 4 {
			t.Fatalf(
				"CloseHandle calls = %d, want two Mutex/root/thread",
				api.count("close"),
			)
		}
	})
}

func TestSet_StartupAndNormalCloseStayOnPinnedThread(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}
	duplicateCalls := api.callsFor("duplicate-thread")
	if len(duplicateCalls) != 1 {
		t.Fatalf(
			"DuplicateHandle calls = %d, want 1",
			len(duplicateCalls),
		)
	}
	pinnedID := duplicateCalls[0].ThreadID
	for _, operation := range []string{"create-mutex", "close"} {
		for _, call := range api.callsFor(operation) {
			if operation == "close" && call.Handle == testThreadHandle {
				continue
			}
			if call.ThreadID != pinnedID {
				t.Fatalf(
					"%s handle %#x thread = %d, want pinned %d",
					operation,
					call.Handle,
					call.ThreadID,
					pinnedID,
				)
			}
		}
	}
	threadCloses := 0
	for _, call := range api.callsFor("close") {
		if call.Handle == testThreadHandle {
			threadCloses++
		}
	}
	if threadCloses != 1 {
		t.Fatalf("thread handle closes = %d, want 1", threadCloses)
	}
}

func TestSet_NewSetWaitsForThreadHandshakeAndBothMutexes(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		index     int
	}{
		{name: "thread handshake", operation: "duplicate-thread", index: 1},
		{name: "second Mutex", operation: "create-mutex", index: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			entered := make(chan struct{})
			proceed := make(chan struct{})
			var once sync.Once
			api.afterCall = func(call apiCall) {
				if call.Operation == test.operation &&
					call.Index == test.index {
					once.Do(func() { close(entered) })
					<-proceed
				}
			}
			result := make(chan *Set, 1)
			errs := make(chan error, 1)
			go func() {
				set, err := newSet(
					t.Context(),
					workerTestLayout(t),
					api,
				)
				result <- set
				errs <- err
			}()
			waitSignal(t, entered, test.name)
			assertPending(t, result, "newSet")
			close(proceed)
			set := waitValue(t, result, "newSet result")
			if err := waitValue(t, errs, "newSet error"); err != nil {
				t.Fatalf("newSet() error = %v", err)
			}
			if err := set.Close(); err != nil {
				t.Fatalf("Set.Close() error = %v", err)
			}
		})
	}
}

func TestSet_StartupFailureClosesCreatedResources(t *testing.T) {
	injectedErr := errors.New("injected startup failure")
	tests := []struct {
		name       string
		configure  func(*testWindowsAPI)
		wantCause  error
		wantText   string
		wantCreate int
		wantClose  int
	}{
		{
			name: "DuplicateHandle",
			configure: func(api *testWindowsAPI) {
				api.duplicateErr = injectedErr
			},
			wantCause:  injectedErr,
			wantCreate: 0,
			wantClose:  1,
		},
		{
			name: "first CreateMutex",
			configure: func(api *testWindowsAPI) {
				api.createMutexErr[1] = injectedErr
			},
			wantCause:  injectedErr,
			wantCreate: 1,
			wantClose:  2,
		},
		{
			name: "first CreateMutex zero handle",
			configure: func(api *testWindowsAPI) {
				api.zeroMutexHandle[1] = true
			},
			wantText:   "create mutex returned zero handle",
			wantCreate: 1,
			wantClose:  2,
		},
		{
			name: "second CreateMutex",
			configure: func(api *testWindowsAPI) {
				api.createMutexErr[2] = injectedErr
			},
			wantCause:  injectedErr,
			wantCreate: 2,
			wantClose:  3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestWindowsAPI()
			test.configure(api)
			_, err := newSet(t.Context(), workerTestLayout(t), api)
			if err == nil {
				t.Fatal("newSet() error = nil, want startup failure")
			}
			if test.wantCause != nil &&
				!errors.Is(err, test.wantCause) {
				t.Fatalf(
					"newSet() error = %v, want cause %v",
					err,
					test.wantCause,
				)
			}
			if test.wantText != "" &&
				!strings.Contains(err.Error(), test.wantText) {
				t.Fatalf(
					"newSet() error = %v, want text %q",
					err,
					test.wantText,
				)
			}
			if got := api.count("create-mutex"); got != test.wantCreate {
				t.Fatalf(
					"CreateMutex calls = %d, want %d",
					got,
					test.wantCreate,
				)
			}
			if got := api.count("close"); got != test.wantClose {
				t.Fatalf(
					"CloseHandle calls = %d, want %d",
					got,
					test.wantClose,
				)
			}
		})
	}
}

func TestSet_NormalCloseIsIdempotent(t *testing.T) {
	api := newTestWindowsAPI()
	set := newWorkerTestSet(t, api)
	const callers = 32
	errs := make(chan error, callers)
	start := make(chan struct{})
	var callersReady sync.WaitGroup
	callersReady.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			callersReady.Done()
			<-start
			errs <- set.Close()
		}()
	}
	waitGroup(t, &callersReady, "Close callers ready")
	close(start)
	for i := 0; i < callers; i++ {
		if err := waitValue(t, errs, "concurrent Set.Close"); err != nil {
			t.Fatalf("Set.Close() error = %v", err)
		}
	}
	if got := api.count("close"); got != 4 {
		t.Fatalf("CloseHandle calls = %d, want 4", got)
	}
}

func TestSet_NormalCloseClosesHandlesInOrderAndJoinsErrors(t *testing.T) {
	api := newTestWindowsAPI()
	backendErr := errors.New("injected backend close failure")
	mutationErr := errors.New("injected mutation close failure")
	rootErr := errors.New("injected root close failure")
	threadErr := errors.New("injected thread close failure")
	api.closeErr = func(call apiCall) error {
		switch call.Handle {
		case testBackendHandle:
			return backendErr
		case testMutationHandle:
			return mutationErr
		case testRootHandle:
			return rootErr
		case testThreadHandle:
			return threadErr
		default:
			return nil
		}
	}
	set := newWorkerTestSet(t, api)
	err := set.Close()
	for _, want := range []error{
		backendErr,
		mutationErr,
		rootErr,
		threadErr,
	} {
		if !errors.Is(err, want) {
			t.Fatalf("Set.Close() error = %v, want joined %v", err, want)
		}
	}
	closeCalls := api.callsFor("close")
	wantHandles := []windows.Handle{
		testBackendHandle,
		testMutationHandle,
		testRootHandle,
		testThreadHandle,
	}
	if len(closeCalls) != len(wantHandles) {
		t.Fatalf(
			"CloseHandle calls = %d, want %d",
			len(closeCalls),
			len(wantHandles),
		)
	}
	for index, want := range wantHandles {
		if closeCalls[index].Handle != want {
			t.Fatalf(
				"CloseHandle[%d] handle = %#x, want %#x",
				index,
				closeCalls[index].Handle,
				want,
			)
		}
	}
}

func TestSet_CloseLinearizesWithAcceptedRequests(t *testing.T) {
	api := newTestWindowsAPI()
	requests := make(chan workerRequest)
	set := &Set{
		requests:  requests,
		closeDone: make(chan struct{}),
		thread:    testThreadHandle,
		api:       api,
	}
	firstAccepted := make(chan struct{})
	allowFirst := make(chan struct{})
	closeAccepted := make(chan struct{})
	allowClose := make(chan struct{})
	firstErr := errors.New("accepted request completed")
	go func() {
		first := <-requests
		close(firstAccepted)
		<-allowFirst
		first.response <- workerResponse{err: firstErr}
		closeRequest := <-requests
		close(closeAccepted)
		<-allowClose
		closeRequest.response <- workerResponse{
			exit: workerExit{},
		}
	}()

	firstResult := make(chan error, 1)
	go func() {
		_, err := set.dispatch(t.Context(), workerRequest{
			operation: requestAcquire,
		})
		firstResult <- err
	}()
	waitSignal(t, firstAccepted, "first request admission")

	closeResult := make(chan error, 1)
	go func() { closeResult <- set.Close() }()
	close(allowFirst)
	if err := waitValue(t, firstResult, "accepted request result"); !errors.Is(err, firstErr) {
		t.Fatalf("first request error = %v, want %v", err, firstErr)
	}
	waitSignal(t, closeAccepted, "Close admission")
	if _, err := set.dispatch(
		t.Context(),
		workerRequest{operation: requestProbe},
	); !errors.Is(err, ErrClosed) {
		t.Fatalf("late dispatch error = %v, want ErrClosed", err)
	}
	close(allowClose)
	if err := waitValue(t, closeResult, "linearized Set.Close"); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}
}

func TestSet_FinishThreadRetriesUntilSignaled(t *testing.T) {
	api := newTestWindowsAPI()
	injectedErr := errors.New("injected thread wait failure")
	finalWaitEntered := make(chan struct{})
	allowFinalWaitReturn := make(chan struct{})
	api.threadWaitResult = func(call apiCall) (uint32, error) {
		switch call.Index {
		case 1:
			return waitResultFailed, injectedErr
		case 2:
			return 0x42, nil
		default:
			return waitResultObject0, nil
		}
	}
	api.afterCall = func(call apiCall) {
		if call.Operation == "wait" &&
			call.Handle == testThreadHandle &&
			call.Index == 3 {
			close(finalWaitEntered)
			<-allowFinalWaitReturn
		}
	}
	set := &Set{thread: testThreadHandle, api: api}
	result := make(chan error, 1)
	go func() {
		result <- set.finishThread(workerExit{waitForThread: true})
	}()
	waitSignal(t, finalWaitEntered, "final WAIT_OBJECT_0")
	assertPending(t, result, "finishThread")
	for _, call := range api.callsFor("close") {
		if call.Handle == testThreadHandle {
			t.Fatal("thread handle closed before final WAIT_OBJECT_0 returned")
		}
	}
	close(allowFinalWaitReturn)
	err := waitValue(t, result, "finishThread")
	if !errors.Is(err, injectedErr) {
		t.Fatalf("finishThread() error = %v, want injected error", err)
	}
	if !strings.Contains(err.Error(), "unexpected wait result") {
		t.Fatalf("finishThread() error = %v, want unexpected result", err)
	}
	if got := api.count("wait"); got != 3 {
		t.Fatalf("thread Wait calls = %d, want 3", got)
	}
	closeCalls := api.callsFor("close")
	if len(closeCalls) != 1 ||
		closeCalls[0].Handle != testThreadHandle {
		t.Fatalf("thread Close calls = %#v, want one final close", closeCalls)
	}
	finalWaitIndex := -1
	closeIndex := -1
	for index, call := range api.callSequence() {
		if call.Operation == "wait" &&
			call.Handle == testThreadHandle &&
			call.Index == 3 {
			finalWaitIndex = index
		}
		if call.Operation == "close" && call.Handle == testThreadHandle {
			closeIndex = index
		}
	}
	if finalWaitIndex < 0 || closeIndex <= finalWaitIndex {
		t.Fatalf(
			"final Wait/Close sequence indexes = %d/%d, want Close after Wait",
			finalWaitIndex,
			closeIndex,
		)
	}
}

func testWorkerOperationErrors(t *testing.T) {
	t.Helper()
	injectedErr := errors.New("injected worker failure")
	tests := []struct {
		name      string
		operation string
		run       func() error
	}{
		{
			name:      "duplicate-worker-thread",
			operation: "duplicate-worker-thread",
			run: func() error {
				api := newTestWindowsAPI()
				api.duplicateErr = injectedErr
				_, err := newSet(t.Context(), workerTestLayout(t), api)
				return err
			},
		},
		{
			name:      "create-mutex",
			operation: "create-mutex",
			run: func() error {
				api := newTestWindowsAPI()
				api.createMutexErr[1] = injectedErr
				_, err := newSet(t.Context(), workerTestLayout(t), api)
				return err
			},
		},
		{
			name:      "close-mutex",
			operation: "close-mutex",
			run: func() error {
				api := newTestWindowsAPI()
				set := newWorkerTestSet(t, api)
				api.closeErr = func(call apiCall) error {
					if call.Handle == testBackendHandle {
						return injectedErr
					}
					return nil
				}
				return set.Close()
			},
		},
		{
			name:      "close-root",
			operation: "close-root",
			run: func() error {
				api := newTestWindowsAPI()
				set := newWorkerTestSet(t, api)
				api.closeErr = func(call apiCall) error {
					if call.Handle == testRootHandle {
						return injectedErr
					}
					return nil
				}
				return set.Close()
			},
		},
		{
			name:      "wait-worker-thread",
			operation: "wait-worker-thread",
			run: func() error {
				api := newTestWindowsAPI()
				api.threadWaitResult = func(call apiCall) (uint32, error) {
					if call.Index == 1 {
						return waitResultFailed, injectedErr
					}
					return waitResultObject0, nil
				}
				set := &Set{thread: testThreadHandle, api: api}
				return set.finishThread(workerExit{waitForThread: true})
			},
		},
		{
			name:      "close-worker-thread",
			operation: "close-worker-thread",
			run: func() error {
				api := newTestWindowsAPI()
				api.closeErr = func(call apiCall) error {
					if call.Handle == testThreadHandle {
						return injectedErr
					}
					return nil
				}
				set := &Set{thread: testThreadHandle, api: api}
				return set.finishThread(workerExit{})
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
