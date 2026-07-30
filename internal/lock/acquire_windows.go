//go:build windows

package lock

import (
	"context"
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Lease 返回本次成功获取创建的租约。
func (r AcquisitionResult) Lease() *Lease {
	return r.lease
}

// Recovered 报告主 Mutex 是否从 abandoned 状态恢复。
func (r AcquisitionResult) Recovered() bool {
	return r.recovered
}

// PeerProbe 返回仍持有主 Mutex 时冻结的对端探测结果。
func (r AcquisitionResult) PeerProbe() ProbeResult {
	return r.peerProbe
}

// AcquireBackend 零等待获取 backend Mutex 并探测 mutation Mutex。
func (s *Set) AcquireBackend(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindBackend)
}

// AcquireMutation 零等待获取 mutation Mutex 并探测 backend Mutex。
func (s *Set) AcquireMutation(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindMutation)
}

func (s *Set) acquire(
	ctx context.Context,
	kind Kind,
) (AcquisitionResult, error) {
	if ctx == nil {
		return AcquisitionResult{}, errors.New("acquire mutex: nil context")
	}
	if err := ctx.Err(); err != nil {
		return AcquisitionResult{}, err
	}
	response, err := s.dispatch(ctx, workerRequest{
		operation: requestAcquire,
		kind:      kind,
	})
	if response.acquisition.lease != nil {
		response.acquisition.lease.set = s
	}
	return response.acquisition, err
}

// Kind 返回租约持有的 Mutex 类型。
func (l *Lease) Kind() Kind {
	return l.kind
}

// Name 返回不含 app-root 路径的稳定 Mutex 名称。
func (l *Lease) Name() string {
	return l.name
}

// Close 在 pinned worker 上幂等释放租约。
func (l *Lease) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.set.release(l.kind, l.generation)
	})
	return l.closeErr
}

func (s *Set) release(kind Kind, generation uint64) error {
	response := make(chan workerResponse, 1)

	s.dispatchMu.Lock()
	s.stateMu.Lock()
	if s.closing || s.closed {
		done := s.closeDone
		closed := s.closed
		s.stateMu.Unlock()
		s.dispatchMu.Unlock()
		if !closed {
			<-done
		}
		return nil
	}
	s.stateMu.Unlock()
	s.requests <- workerRequest{
		operation:  requestRelease,
		kind:       kind,
		generation: generation,
		response:   response,
	}
	s.dispatchMu.Unlock()

	result := <-response
	return result.err
}

func (s *workerState) acquire(request workerRequest) workerResponse {
	var result AcquisitionResult
	if s.poison != nil {
		return workerResponse{
			acquisition: result,
			err:         &PoisonedError{Cause: s.poison},
		}
	}
	if err := request.ctx.Err(); err != nil {
		return workerResponse{acquisition: result, err: err}
	}
	if !request.kind.Valid() {
		return workerResponse{
			acquisition: result,
			err:         fmt.Errorf("invalid mutex kind %q", request.kind),
		}
	}
	if slot, code, conflict := s.activeConflict(request.kind); conflict {
		return workerResponse{
			acquisition: result,
			err:         conflictError(slot, code),
		}
	}

	primary := s.slot(request.kind)
	peer := s.peerSlot(request.kind)
	cleanupPrimary := func(cause error) workerResponse {
		if !primary.maybeOwned {
			return workerResponse{acquisition: result, err: cause}
		}
		releaseErr := s.releaseSlot(primary, "release-primary")
		if ctxErr := request.ctx.Err(); ctxErr != nil &&
			!errors.Is(cause, ctxErr) {
			cause = errors.Join(ctxErr, cause)
		}
		return workerResponse{
			acquisition: result,
			err:         errors.Join(cause, releaseErr),
		}
	}

	primaryWait := s.observeWait(primary)
	if primaryWait.result == waitResultAbandoned {
		result.recovered = true
	}
	if err := request.ctx.Err(); err != nil {
		return cleanupPrimary(err)
	}
	if primaryWait.err != nil {
		return cleanupPrimary(waitOperationError(
			"wait-primary",
			primary,
			primaryWait,
		))
	}
	switch primaryWait.result {
	case waitResultTimeout:
		return workerResponse{
			acquisition: result,
			err:         s.primaryConflict(request.kind),
		}
	case waitResultObject0, waitResultAbandoned:
	default:
		return cleanupPrimary(waitOperationError(
			"wait-primary",
			primary,
			primaryWait,
		))
	}

	peerProbe, err := s.probeSlot(
		request.ctx,
		peer,
		"wait-peer",
		"release-peer",
	)
	result.peerProbe = peerProbe
	if err != nil {
		return cleanupPrimary(err)
	}
	if err := request.ctx.Err(); err != nil {
		return cleanupPrimary(err)
	}
	if peerProbe.Held {
		return cleanupPrimary(s.peerConflict(request.kind))
	}

	s.nextGeneration++
	s.activeKind = request.kind
	s.activeGeneration = s.nextGeneration
	result.lease = &Lease{
		kind:       request.kind,
		name:       primary.name,
		generation: s.activeGeneration,
	}
	return workerResponse{acquisition: result}
}

type waitObservation struct {
	result uint32
	err    error
}

func (s *workerState) observeWait(slot *mutexState) waitObservation {
	result, err := s.api.waitForSingleObject(
		slot.handle,
		mutexWaitTimeout,
	)
	if result == waitResultObject0 ||
		result == waitResultAbandoned {
		slot.maybeOwned = true
	}
	return waitObservation{result: result, err: err}
}

func waitOperationError(
	operation string,
	slot *mutexState,
	observation waitObservation,
) error {
	cause := observation.err
	if cause == nil {
		cause = fmt.Errorf(
			"unexpected wait result %#x",
			observation.result,
		)
	}
	return &OperationError{
		code:      protocol.CodeMutexOperationFailed,
		Operation: operation,
		Kind:      slot.kind,
		Name:      slot.name,
		Cause:     cause,
	}
}

func (s *workerState) probeSlot(
	ctx context.Context,
	slot *mutexState,
	waitOperation string,
	releaseOperation string,
) (ProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return ProbeResult{}, err
	}

	observation := s.observeWait(slot)
	var result ProbeResult
	switch observation.result {
	case waitResultObject0:
		result = ProbeResult{Held: false, Recovered: false}
	case waitResultAbandoned:
		result = ProbeResult{Held: false, Recovered: true}
	}

	// Wait 的原始结果已经冻结；取消必须先于冲突或 Win32 故障解释。
	if err := ctx.Err(); err != nil {
		var releaseErr error
		if slot.maybeOwned {
			releaseErr = s.releaseSlot(slot, releaseOperation)
		}
		return result, errors.Join(err, releaseErr)
	}
	if observation.err != nil {
		waitErr := waitOperationError(waitOperation, slot, observation)
		var releaseErr error
		if slot.maybeOwned {
			releaseErr = s.releaseSlot(slot, releaseOperation)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, errors.Join(ctxErr, waitErr, releaseErr)
		}
		return result, errors.Join(waitErr, releaseErr)
	}

	switch observation.result {
	case waitResultTimeout:
		result = ProbeResult{Held: true}
	case waitResultObject0, waitResultAbandoned:
		releaseErr := s.releaseSlot(slot, releaseOperation)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, errors.Join(ctxErr, releaseErr)
		}
		if releaseErr != nil {
			return result, releaseErr
		}
	default:
		return ProbeResult{}, waitOperationError(
			waitOperation,
			slot,
			observation,
		)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *workerState) releaseSlot(slot *mutexState, operation string) error {
	if !slot.maybeOwned {
		return nil
	}
	if err := s.api.releaseMutex(slot.handle); err != nil {
		operationErr := &OperationError{
			code:      protocol.CodeMutexOperationFailed,
			Operation: operation,
			Kind:      slot.kind,
			Name:      slot.name,
			Cause:     err,
		}
		return s.poisonWith(operationErr)
	}
	slot.maybeOwned = false
	return nil
}

func (s *workerState) slot(kind Kind) *mutexState {
	switch kind {
	case KindBackend:
		return &s.backend
	case KindMutation:
		return &s.mutation
	default:
		return nil
	}
}

func (s *workerState) peerSlot(kind Kind) *mutexState {
	switch kind {
	case KindBackend:
		return &s.mutation
	case KindMutation:
		return &s.backend
	default:
		return nil
	}
}

func (s *workerState) activeConflict(
	requested Kind,
) (*mutexState, protocol.Code, bool) {
	switch s.activeKind {
	case "":
		return nil, "", false
	case KindBackend:
		if requested == KindBackend {
			return &s.backend, protocol.CodeBackendAlreadyRunning, true
		}
		return &s.backend, protocol.CodeBackendStillRunning, true
	case KindMutation:
		return &s.mutation, protocol.CodeMutationInProgress, true
	default:
		return nil, "", false
	}
}

func (s *workerState) primaryConflict(requested Kind) error {
	if requested == KindBackend {
		return conflictError(
			&s.backend,
			protocol.CodeBackendAlreadyRunning,
		)
	}
	return conflictError(
		&s.mutation,
		protocol.CodeMutationInProgress,
	)
}

func (s *workerState) peerConflict(requested Kind) error {
	if requested == KindBackend {
		return conflictError(
			&s.mutation,
			protocol.CodeMutationInProgress,
		)
	}
	return conflictError(
		&s.backend,
		protocol.CodeBackendStillRunning,
	)
}

func conflictError(slot *mutexState, code protocol.Code) error {
	return &ConflictError{
		code: code,
		Kind: slot.kind,
		Name: slot.name,
	}
}

func (s *workerState) release(request workerRequest) workerResponse {
	if s.poison != nil {
		return workerResponse{
			err: &PoisonedError{Cause: s.poison},
		}
	}
	if s.activeKind != request.kind ||
		s.activeGeneration != request.generation {
		return workerResponse{}
	}
	slot := s.slot(request.kind)
	if err := s.releaseSlot(slot, "release-lease"); err != nil {
		return workerResponse{err: err}
	}
	s.activeKind = ""
	s.activeGeneration = 0
	return workerResponse{}
}

func (s *workerState) poisonWith(cause error) error {
	if s.poison == nil {
		s.poison = cause
		return &PoisonedError{Cause: s.poison}
	}
	return errors.Join(
		&PoisonedError{Cause: s.poison},
		cause,
	)
}

func (s *workerState) releaseSlotForClose(
	slot *mutexState,
) error {
	if !slot.maybeOwned {
		return nil
	}
	if err := s.api.releaseMutex(slot.handle); err != nil {
		return &OperationError{
			code:      protocol.CodeMutexOperationFailed,
			Operation: "release-on-close",
			Kind:      slot.kind,
			Name:      slot.name,
			Cause:     err,
		}
	}
	slot.maybeOwned = false
	return nil
}
