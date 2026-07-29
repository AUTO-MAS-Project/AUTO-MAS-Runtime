package protocol

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockObservingRenderer struct {
	output   *ProcessOutput
	calls    []EventType
	unlocked []EventType
}

type blockingResultRenderer struct {
	output                 *ProcessOutput
	entered                chan struct{}
	release                chan struct{}
	calls                  atomic.Int32
	terminalDuringRenderer atomic.Bool
}

func (r *lockObservingRenderer) observe(eventType EventType) error {
	r.calls = append(r.calls, eventType)
	if r.output.mu.TryLock() {
		r.output.mu.Unlock()
		r.unlocked = append(r.unlocked, eventType)
	}
	return nil
}

func (r *lockObservingRenderer) RenderHello(HelloEvent) error {
	return r.observe(TypeHello)
}

func (r *lockObservingRenderer) RenderProgress(ProgressEvent) error {
	return r.observe(TypeProgress)
}

func (r *lockObservingRenderer) RenderState(StateEvent) error {
	return r.observe(TypeState)
}

func (r *lockObservingRenderer) RenderLog(LogEvent) error {
	return r.observe(TypeLog)
}

func (r *lockObservingRenderer) RenderWarning(WarningEvent) error {
	return r.observe(TypeWarning)
}

func (r *lockObservingRenderer) RenderError(ErrorEvent) error {
	return r.observe(TypeError)
}

func (r *lockObservingRenderer) RenderResult(ResultEvent) error {
	return r.observe(TypeResult)
}

func (r *blockingResultRenderer) RenderHello(HelloEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderProgress(ProgressEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderState(StateEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderLog(LogEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderWarning(WarningEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderError(ErrorEvent) error {
	return nil
}

func (r *blockingResultRenderer) RenderResult(ResultEvent) error {
	r.terminalDuringRenderer.Store(r.output.terminal)
	if r.calls.Add(1) == 1 {
		close(r.entered)
		<-r.release
	}
	return nil
}

func TestEmitterRendererRunsWithinSerializationLock(t *testing.T) {
	renderer := &lockObservingRenderer{}
	output, err := NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	renderer.output = output
	emitter, err := output.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	if err := emitter.EmitProgress(ProgressEvent{}); err != nil {
		t.Fatalf("EmitProgress() error = %v", err)
	}
	if err := emitter.EmitState(StateEvent{}); err != nil {
		t.Fatalf("EmitState() error = %v", err)
	}
	if err := emitter.EmitLog(LogEvent{}); err != nil {
		t.Fatalf("EmitLog() error = %v", err)
	}
	if err := emitter.EmitWarning(WarningEvent{}); err != nil {
		t.Fatalf("EmitWarning() error = %v", err)
	}
	if err := emitter.EmitError(ErrorEvent{}); err != nil {
		t.Fatalf("EmitError() error = %v", err)
	}
	if err := emitter.EmitResult(ResultEvent{}); err != nil {
		t.Fatalf("EmitResult() error = %v", err)
	}

	wantCalls := []EventType{
		TypeHello,
		TypeProgress,
		TypeState,
		TypeLog,
		TypeWarning,
		TypeError,
		TypeResult,
	}
	if !reflect.DeepEqual(renderer.calls, wantCalls) {
		t.Fatalf("renderer calls = %#v, want %#v", renderer.calls, wantCalls)
	}
	if len(renderer.unlocked) != 0 {
		t.Fatalf("renderer calls outside serialization lock = %#v, want none", renderer.unlocked)
	}
}

// 阻塞首个 result renderer，并在仍持有串行化锁时观察终态预约。
// renderer 内的终态观察与锁断言共同证明 terminal 在解锁前提交，随后验证第二个 result 被拒绝。
func TestEmitterResultCommitPrecedesSerializationUnlock(t *testing.T) {
	renderer := &blockingResultRenderer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseRenderer := func() {
		releaseOnce.Do(func() {
			close(renderer.release)
		})
	}
	t.Cleanup(releaseRenderer)

	output, err := NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	renderer.output = output
	emitter, err := output.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	results := make(chan error, 1)
	go func() {
		results <- emitter.EmitResult(ResultEvent{})
	}()

	entryTimeout := time.NewTimer(2 * time.Second)
	defer entryTimeout.Stop()
	select {
	case <-renderer.entered:
	case <-entryTimeout.C:
		t.Fatal("first result did not enter renderer")
	}
	if !renderer.terminalDuringRenderer.Load() {
		t.Fatal("terminal = false during RenderResult(), want reserved before renderer and unlock")
	}
	if output.mu.TryLock() {
		output.mu.Unlock()
		t.Fatal("serialization lock was available while RenderResult() was blocked")
	}
	select {
	case err := <-results:
		t.Fatalf("EmitResult() completed while first renderer was blocked: %v", err)
	default:
	}

	releaseRenderer()
	resultTimeout := time.NewTimer(2 * time.Second)
	defer resultTimeout.Stop()
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("first EmitResult() error = %v", err)
		}
	case <-resultTimeout.C:
		t.Fatal("first EmitResult() did not finish")
	}
	if err := emitter.EmitResult(ResultEvent{}); !errors.Is(err, ErrResultAlreadyEmitted) {
		t.Fatalf("second EmitResult() error = %v, want ErrResultAlreadyEmitted", err)
	}
	if got := renderer.calls.Load(); got != 1 {
		t.Fatalf("RenderResult() calls = %d, want 1", got)
	}
}

func TestEmitterResultStickyFailureRollsBackReservation(t *testing.T) {
	sentinel := errors.New("result renderer sentinel")
	renderer := &internalCountingRenderer{
		delegate: &internalFailingRenderer{
			errAt: TypeResult,
			err:   sentinel,
		},
	}
	output, err := NewProcessOutputWithRenderer(renderer)
	if err != nil {
		t.Fatalf("NewProcessOutputWithRenderer() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"v1.0.0",
		"doctor",
		nil,
		WithOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	firstErr := emitter.EmitResult(ResultEvent{})
	if !errors.Is(firstErr, sentinel) {
		t.Fatalf("first EmitResult() error = %v, want renderer sentinel", firstErr)
	}
	if output.terminal {
		t.Fatal("terminal = true after failed result renderer, want reservation rolled back")
	}
	if output.nextSequence != 2 {
		t.Fatalf("nextSequence = %d after failed result renderer, want 2", output.nextSequence)
	}
	if output.writeErr != firstErr {
		t.Fatalf("writeErr = %v, want same sticky error value %v", output.writeErr, firstErr)
	}

	callsAfterFailure := append([]EventType(nil), renderer.calls...)
	secondErr := emitter.EmitResult(ResultEvent{})
	if secondErr != firstErr {
		t.Fatalf("second EmitResult() error = %v, want same sticky error value %v", secondErr, firstErr)
	}
	if !reflect.DeepEqual(renderer.calls, callsAfterFailure) {
		t.Fatalf("renderer calls after sticky result failure = %#v, want unchanged %#v", renderer.calls, callsAfterFailure)
	}
	if output.terminal {
		t.Fatal("terminal = true after repeated sticky error, want false")
	}
	if output.nextSequence != 2 {
		t.Fatalf("nextSequence = %d after repeated sticky error, want 2", output.nextSequence)
	}
}
