//go:build windows

package lock

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type requestKind uint8

const (
	requestAcquire requestKind = iota + 1
	requestProbe
	requestRelease
	requestClose
)

type workerRequest struct {
	operation  requestKind
	ctx        context.Context
	kind       Kind
	generation uint64
	response   chan workerResponse
}

type workerResponse struct {
	acquisition AcquisitionResult
	probe       ProbeResult
	exit        workerExit
	err         error
}

type threadHandshake struct {
	threadHandle windows.Handle
	recorded     chan struct{}
	err          error
}

type workerReady struct {
	err error
}

type workerExit struct {
	waitForThread bool
	err           error
}

type mutexState struct {
	kind       Kind
	name       string
	handle     windows.Handle
	maybeOwned bool
}

type workerState struct {
	api              windowsAPI
	rootHandle       windows.Handle
	backend          mutexState
	mutation         mutexState
	activeKind       Kind
	activeGeneration uint64
	nextGeneration   uint64
	poison           error
}

// ProbeResult 描述一次零等待探测观察到的状态。
type ProbeResult struct {
	Held      bool
	Recovered bool
}

// AcquisitionResult 保留获取结果以及错误路径上的恢复元数据。
type AcquisitionResult struct {
	lease     *Lease
	recovered bool
	peerProbe ProbeResult
}

// Lease 代表 pinned worker 当前持有的一把 Mutex。
type Lease struct {
	set        *Set
	kind       Kind
	name       string
	generation uint64
	closeOnce  sync.Once
	closeErr   error
}

// Set 管理同一物理 app root 的两把 Windows 命名 Mutex。
type Set struct {
	// dispatchMu 保护请求入队与关闭请求入队的线性化顺序。
	dispatchMu sync.Mutex
	requests   chan workerRequest

	// stateMu 保护 closing、closed、closeDone 的关闭时机和 closeErr。
	stateMu   sync.Mutex
	closing   bool
	closed    bool
	closeDone chan struct{}
	closeErr  error

	thread windows.Handle
	api    windowsAPI
}

// NewSet 打开并固定 layout 的物理 app root，再启动 pinned worker。
func NewSet(ctx context.Context, layout *config.Layout) (*Set, error) {
	return newSet(ctx, layout, systemWindowsAPI{})
}

func newSet(
	ctx context.Context,
	layout *config.Layout,
	api windowsAPI,
) (*Set, error) {
	if ctx == nil {
		return nil, errors.New("new mutex set: nil context")
	}
	if layout == nil {
		return nil, errors.New("new mutex set: nil layout")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openRootIdentity(ctx, api, layout.AppRoot())
	if err != nil {
		return nil, err
	}
	closeRootOnFailure := func(cause error) (*Set, error) {
		closeErr := api.closeHandle(root.handle)
		if closeErr != nil {
			closeErr = &OperationError{
				code:      protocol.CodeMutexOperationFailed,
				Operation: "close-root",
				Cause:     closeErr,
			}
		}
		return nil, errors.Join(cause, closeErr)
	}
	backendName, err := mutexName(KindBackend, root.identity)
	if err != nil {
		return closeRootOnFailure(err)
	}
	mutationName, err := mutexName(KindMutation, root.identity)
	if err != nil {
		return closeRootOnFailure(err)
	}

	handshake := make(chan threadHandshake, 1)
	ready := make(chan workerReady, 1)
	set := &Set{
		requests:  make(chan workerRequest),
		closeDone: make(chan struct{}),
		api:       api,
	}
	go runWorker(
		ctx,
		api,
		root,
		backendName,
		mutationName,
		handshake,
		ready,
		set.requests,
	)
	thread := <-handshake
	if thread.err != nil {
		return nil, thread.err
	}
	set.thread = thread.threadHandle
	close(thread.recorded)

	started := <-ready
	if started.err != nil {
		threadCloseErr := api.closeHandle(set.thread)
		if threadCloseErr != nil {
			threadCloseErr = &OperationError{
				code:      protocol.CodeMutexOperationFailed,
				Operation: "close-worker-thread",
				Cause:     threadCloseErr,
			}
		}
		return nil, errors.Join(started.err, threadCloseErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, set.Close())
	}
	return set, nil
}

func (s *Set) dispatch(
	ctx context.Context,
	request workerRequest,
) (workerResponse, error) {
	if ctx == nil {
		return workerResponse{}, errors.New("mutex request context is nil")
	}
	if err := ctx.Err(); err != nil {
		return workerResponse{}, err
	}
	request.ctx = ctx
	request.response = make(chan workerResponse, 1)

	s.dispatchMu.Lock()
	s.stateMu.Lock()
	if s.closing || s.closed {
		s.stateMu.Unlock()
		s.dispatchMu.Unlock()
		return workerResponse{}, ErrClosed
	}
	s.stateMu.Unlock()
	select {
	case s.requests <- request:
		s.dispatchMu.Unlock()
	case <-ctx.Done():
		s.dispatchMu.Unlock()
		return workerResponse{}, ctx.Err()
	}

	// 请求一旦入队必须等待 worker 收口，避免取消遗留已取得的 Mutex。
	response := <-request.response
	return response, response.err
}

// Close 线性化关闭请求，释放资源并缓存完整收口结果。
func (s *Set) Close() error {
	s.dispatchMu.Lock()
	s.stateMu.Lock()
	if s.closed {
		err := s.closeErr
		s.stateMu.Unlock()
		s.dispatchMu.Unlock()
		return err
	}
	if s.closing {
		done := s.closeDone
		s.stateMu.Unlock()
		s.dispatchMu.Unlock()
		<-done
		s.stateMu.Lock()
		err := s.closeErr
		s.stateMu.Unlock()
		return err
	}
	s.closing = true
	response := make(chan workerResponse, 1)
	s.stateMu.Unlock()
	s.requests <- workerRequest{
		operation: requestClose,
		response:  response,
	}
	s.dispatchMu.Unlock()

	result := <-response
	closeErr := s.finishThread(result.exit)
	s.stateMu.Lock()
	s.closeErr = errors.Join(result.err, closeErr)
	s.closed = true
	close(s.closeDone)
	err := s.closeErr
	s.stateMu.Unlock()
	return err
}

func (s *Set) finishThread(exit workerExit) error {
	var waitErr error
	if exit.waitForThread {
		for {
			result, err := s.api.waitForSingleObject(s.thread, threadWait)
			if err != nil {
				waitErr = errors.Join(waitErr, &OperationError{
					code:      protocol.CodeMutexOperationFailed,
					Operation: "wait-worker-thread",
					Cause:     err,
				})
				runtime.Gosched()
				continue
			}
			if result != waitResultObject0 {
				waitErr = errors.Join(waitErr, &OperationError{
					code:      protocol.CodeMutexOperationFailed,
					Operation: "wait-worker-thread",
					Cause: fmt.Errorf(
						"unexpected wait result %#x",
						result,
					),
				})
				runtime.Gosched()
				continue
			}
			break
		}
	}
	threadCloseErr := s.api.closeHandle(s.thread)
	if threadCloseErr != nil {
		threadCloseErr = &OperationError{
			code:      protocol.CodeMutexOperationFailed,
			Operation: "close-worker-thread",
			Cause:     threadCloseErr,
		}
	}
	return errors.Join(exit.err, waitErr, threadCloseErr)
}

func runWorker(
	ctx context.Context,
	api windowsAPI,
	root rootResource,
	backendName string,
	mutationName string,
	handshake chan<- threadHandshake,
	ready chan<- workerReady,
	requests <-chan workerRequest,
) {
	runtime.LockOSThread()
	currentProcess := api.currentProcess()
	currentThread := api.currentThread()
	var thread windows.Handle
	err := api.duplicateHandle(
		currentProcess,
		currentThread,
		currentProcess,
		&thread,
		threadAccess,
		false,
		0,
	)
	if err != nil {
		rootCloseErr := closeRootResource(api, root.handle)
		runtime.UnlockOSThread()
		handshake <- threadHandshake{err: errors.Join(
			&OperationError{
				code:      protocol.CodeMutexOperationFailed,
				Operation: "duplicate-worker-thread",
				Cause:     err,
			},
			rootCloseErr,
		)}
		return
	}
	recorded := make(chan struct{})
	handshake <- threadHandshake{
		threadHandle: thread,
		recorded:     recorded,
	}
	<-recorded

	state, err := createWorkerState(
		ctx,
		api,
		root.handle,
		backendName,
		mutationName,
	)
	if err != nil {
		runtime.UnlockOSThread()
		ready <- workerReady{err: err}
		return
	}
	ready <- workerReady{}

	for {
		request := <-requests
		switch request.operation {
		case requestAcquire:
			request.response <- state.acquire(request)
		case requestRelease:
			request.response <- state.release(request)
		case requestClose:
			finishWorker(state, request.response)
			return
		default:
			request.response <- workerResponse{
				err: fmt.Errorf(
					"unsupported worker request %d",
					request.operation,
				),
			}
		}
	}
}

func createWorkerState(
	ctx context.Context,
	api windowsAPI,
	rootHandle windows.Handle,
	backendName string,
	mutationName string,
) (*workerState, error) {
	state := &workerState{
		api:        api,
		rootHandle: rootHandle,
		backend: mutexState{
			kind: KindBackend,
			name: backendName,
		},
		mutation: mutexState{
			kind: KindMutation,
			name: mutationName,
		},
	}
	create := func(slot *mutexState) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := windows.UTF16PtrFromString(slot.name)
		if err != nil {
			return &OperationError{
				code:      protocol.CodeMutexOperationFailed,
				Operation: "encode-mutex-name",
				Kind:      slot.kind,
				Name:      slot.name,
				Cause:     err,
			}
		}
		handle, err := api.createMutex(nil, false, name)
		if handle == 0 ||
			(err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
			cause := err
			if cause == nil {
				cause = errors.New("create mutex returned zero handle")
			}
			return &OperationError{
				code:      protocol.CodeMutexOperationFailed,
				Operation: "create-mutex",
				Kind:      slot.kind,
				Name:      slot.name,
				Cause:     cause,
			}
		}
		slot.handle = handle
		return nil
	}
	if err := create(&state.backend); err != nil {
		return nil, errors.Join(err, closeRootHandle(state))
	}
	if err := create(&state.mutation); err != nil {
		return nil, errors.Join(
			err,
			closeMutexHandle(state, &state.backend),
			closeRootHandle(state),
		)
	}
	return state, nil
}

func closeMutexHandle(state *workerState, slot *mutexState) error {
	if slot.handle == 0 {
		return nil
	}
	handle := slot.handle
	slot.handle = 0
	if err := state.api.closeHandle(handle); err != nil {
		return &OperationError{
			code:      protocol.CodeMutexOperationFailed,
			Operation: "close-mutex",
			Kind:      slot.kind,
			Name:      slot.name,
			Cause:     err,
		}
	}
	return nil
}

func closeRootHandle(state *workerState) error {
	if state.rootHandle == 0 {
		return nil
	}
	handle := state.rootHandle
	state.rootHandle = 0
	return closeRootResource(state.api, handle)
}

func closeRootResource(api windowsAPI, handle windows.Handle) error {
	if handle == 0 {
		return nil
	}
	if err := api.closeHandle(handle); err != nil {
		return &OperationError{
			code:      protocol.CodeMutexOperationFailed,
			Operation: "close-root",
			Cause:     err,
		}
	}
	return nil
}

func closeWorkerHandles(state *workerState) error {
	return errors.Join(
		closeMutexHandle(state, &state.backend),
		closeMutexHandle(state, &state.mutation),
		closeRootHandle(state),
	)
}

func finishWorker(
	state *workerState,
	response chan workerResponse,
) {
	backendRelease := state.releaseSlotForClose(&state.backend)
	mutationRelease := state.releaseSlotForClose(&state.mutation)
	closeErr := closeWorkerHandles(state)
	allErr := errors.Join(
		state.poison,
		backendRelease,
		mutationRelease,
		closeErr,
	)
	if !state.backend.maybeOwned && !state.mutation.maybeOwned {
		runtime.UnlockOSThread()
		response <- workerResponse{
			exit: workerExit{waitForThread: false, err: allErr},
		}
		return
	}
	response <- workerResponse{
		exit: workerExit{waitForThread: true, err: allErr},
	}
	// 所有权不确定时退出 goroutine，让 Go 终止仍锁定的 OS thread。
}
