//go:build windows

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type countingStartupAPI struct {
	windowsAPI

	mu               sync.Mutex
	duplicateCalls   int
	createMutexCalls int
}

func (a *countingStartupAPI) duplicateHandle(
	sourceProcess windows.Handle,
	source windows.Handle,
	targetProcess windows.Handle,
	target *windows.Handle,
	desiredAccess uint32,
	inheritHandle bool,
	options uint32,
) error {
	a.mu.Lock()
	a.duplicateCalls++
	a.mu.Unlock()
	return a.windowsAPI.duplicateHandle(
		sourceProcess,
		source,
		targetProcess,
		target,
		desiredAccess,
		inheritHandle,
		options,
	)
}

func (a *countingStartupAPI) createMutex(
	security *windows.SecurityAttributes,
	initialOwner bool,
	name *uint16,
) (windows.Handle, error) {
	a.mu.Lock()
	a.createMutexCalls++
	a.mu.Unlock()
	return a.windowsAPI.createMutex(security, initialOwner, name)
}

func (a *countingStartupAPI) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.duplicateCalls, a.createMutexCalls
}

func TestNewSet_RejectsNilInputsAndMissingRoot(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if _, err := NewSet(nil, layout); err == nil {
		t.Fatal("NewSet(nil context) error = nil, want rejection")
	}
	if _, err := NewSet(t.Context(), nil); err == nil {
		t.Fatal("NewSet(nil layout) error = nil, want rejection")
	}

	missing := filepath.Join(t.TempDir(), "missing")
	missingLayout, err := config.NewLayout(
		missing,
		filepath.Dir(missing),
	)
	if err != nil {
		t.Fatalf("config.NewLayout(missing) error = %v", err)
	}
	if _, err := NewSet(t.Context(), missingLayout); err == nil {
		t.Fatal("NewSet(missing root) error = nil, want rejection")
	}
}

func TestNewSet_RejectsRealFileBeforeWorker(t *testing.T) {
	parent := t.TempDir()
	appRoot := filepath.Join(parent, "app-root.txt")
	if err := os.WriteFile(appRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, parent)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	api := &countingStartupAPI{windowsAPI: systemWindowsAPI{}}
	set, err := newSet(t.Context(), layout, api)
	if err == nil {
		if closeErr := set.Close(); closeErr != nil {
			t.Errorf("Set.Close() error = %v", closeErr)
		}
		t.Fatal("newSet() error = nil, want regular-file rejection")
	}
	duplicateCalls, createMutexCalls := api.counts()
	if duplicateCalls != 0 || createMutexCalls != 0 {
		t.Fatalf(
			"DuplicateHandle/CreateMutex calls = %d/%d, want 0/0",
			duplicateCalls,
			createMutexCalls,
		)
	}
}

func TestNewSet_OpensExistingNamedMutex(t *testing.T) {
	appRoot := t.TempDir()
	layout, err := config.NewLayout(appRoot, filepath.Dir(appRoot))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	first, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("first NewSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("cleanup first Set.Close() error = %v", err)
		}
	})
	second, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("second NewSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("cleanup second Set.Close() error = %v", err)
		}
	})
	if err := second.Close(); err != nil {
		t.Fatalf("second Set.Close() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Set.Close() error = %v", err)
	}
}

type releaseFailureAPI struct {
	windowsAPI

	fail               atomic.Bool
	failedReleaseCalls atomic.Int32
	waitEnteredOnce    sync.Once
	allowWaitOnce      sync.Once
	threadWaitEntered  chan struct{}
	allowThreadWait    chan struct{}
	releaseErr         error
}

func (a *releaseFailureAPI) releaseMutex(handle windows.Handle) error {
	if a.fail.Load() {
		a.failedReleaseCalls.Add(1)
		return a.releaseErr
	}
	return a.windowsAPI.releaseMutex(handle)
}

func (a *releaseFailureAPI) waitForSingleObject(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (uint32, error) {
	if timeoutMilliseconds == threadWait && a.fail.Load() {
		a.waitEnteredOnce.Do(func() { close(a.threadWaitEntered) })
		<-a.allowThreadWait
	}
	return a.windowsAPI.waitForSingleObject(handle, timeoutMilliseconds)
}

func (a *releaseFailureAPI) allowThreadTerminationWait() {
	a.allowWaitOnce.Do(func() { close(a.allowThreadWait) })
}

type recordedWait struct {
	handle  windows.Handle
	timeout uint32
	result  uint32
	err     error
}

type recordingWindowsAPI struct {
	windowsAPI

	mu    sync.Mutex
	waits []recordedWait
}

func (a *recordingWindowsAPI) waitForSingleObject(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (uint32, error) {
	result, err := a.windowsAPI.waitForSingleObject(
		handle,
		timeoutMilliseconds,
	)
	a.mu.Lock()
	a.waits = append(a.waits, recordedWait{
		handle:  handle,
		timeout: timeoutMilliseconds,
		result:  result,
		err:     err,
	})
	a.mu.Unlock()
	return result, err
}

func (a *recordingWindowsAPI) recordedWaits() []recordedWait {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedWait(nil), a.waits...)
}

func TestSet_CloseWithActiveLeaseReleaseFailureTerminatesThread(
	t *testing.T,
) {
	harness := newUncertainOwnershipHarness(t)
	harness.firstAPI.fail.Store(true)

	harness.closeFirstAndAssertAbandoned(t, 1)
	if err := harness.lease.Close(); err != nil {
		t.Fatalf("old Lease.Close() error = %v, want nil", err)
	}
}

func TestSet_CloseTerminatesPinnedThreadWhenOwnershipUncertain(
	t *testing.T,
) {
	harness := newUncertainOwnershipHarness(t)
	harness.firstAPI.fail.Store(true)
	leaseErr := harness.lease.Close()
	if !errors.Is(leaseErr, harness.injectedErr) ||
		!errors.Is(leaseErr, ErrPoisoned) {
		t.Fatalf(
			"Lease.Close() error = %v, want poison and injected failure",
			leaseErr,
		)
	}

	harness.closeFirstAndAssertAbandoned(t, 2)
}

type uncertainOwnershipHarness struct {
	injectedErr error
	firstAPI    *releaseFailureAPI
	first       *Set
	lease       *Lease
	secondAPI   *recordingWindowsAPI
	second      *Set
}

func newUncertainOwnershipHarness(
	t *testing.T,
) *uncertainOwnershipHarness {
	t.Helper()
	injectedErr := errors.New("injected real release mutex failure")
	appRoot := t.TempDir()
	layout, err := config.NewLayout(appRoot, filepath.Dir(appRoot))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}

	firstAPI := &releaseFailureAPI{
		windowsAPI:        systemWindowsAPI{},
		threadWaitEntered: make(chan struct{}),
		allowThreadWait:   make(chan struct{}),
		releaseErr:        injectedErr,
	}
	first, err := newSet(t.Context(), layout, firstAPI)
	if err != nil {
		t.Fatalf("first newSet() error = %v", err)
	}
	t.Cleanup(func() {
		// cleanup 必须先解除协调方等待，才可进入可能阻塞的 Close。
		firstAPI.allowThreadTerminationWait()
		_ = first.Close()
	})

	secondAPI := &recordingWindowsAPI{windowsAPI: systemWindowsAPI{}}
	second, err := newSet(t.Context(), layout, secondAPI)
	if err != nil {
		t.Fatalf("second newSet() error = %v", err)
	}
	t.Cleanup(func() {
		// 主断言负责错误；cleanup 只保证幂等收口。
		_ = second.Close()
	})

	acquired, err := first.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("first AcquireBackend() error = %v", err)
	}
	if acquired.Lease() == nil {
		t.Fatal("first Lease() = nil, want active Lease")
	}

	return &uncertainOwnershipHarness{
		injectedErr: injectedErr,
		firstAPI:    firstAPI,
		first:       first,
		lease:       acquired.Lease(),
		secondAPI:   secondAPI,
		second:      second,
	}
}

func (h *uncertainOwnershipHarness) closeFirstAndAssertAbandoned(
	t *testing.T,
	wantFailedReleases int32,
) {
	t.Helper()
	closeResult := make(chan error, 1)
	go func() { closeResult <- h.first.Close() }()
	waitSignal(t, h.firstAPI.threadWaitEntered, "thread-handle wait")
	assertPending(t, closeResult, "Set.Close")
	h.firstAPI.allowThreadTerminationWait()
	closeErr := waitValue(t, closeResult, "uncertain ownership Set.Close")
	if !errors.Is(closeErr, h.injectedErr) {
		t.Fatalf(
			"Set.Close() error = %v, want injected failure",
			closeErr,
		)
	}
	if got := h.firstAPI.failedReleaseCalls.Load(); got != wantFailedReleases {
		t.Fatalf(
			"failed Release calls = %d, want %d",
			got,
			wantFailedReleases,
		)
	}

	recovered, err := h.second.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("second AcquireBackend() error = %v", err)
	}
	if !recovered.Recovered() {
		t.Fatal("second Recovered() = false, want true")
	}
	waits := h.secondAPI.recordedWaits()
	if len(waits) == 0 ||
		waits[0].timeout != mutexWaitTimeout ||
		waits[0].result != waitResultAbandoned ||
		waits[0].err != nil {
		t.Fatalf(
			"second first zero Wait = %#v, want WAIT_ABANDONED",
			waits,
		)
	}
	if err := recovered.Lease().Close(); err != nil {
		t.Fatalf("recovered Lease.Close() error = %v", err)
	}
	if err := h.second.Close(); err != nil {
		t.Fatalf("second Set.Close() error = %v", err)
	}
}
