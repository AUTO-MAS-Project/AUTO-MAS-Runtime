package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeProvider struct {
	mu          sync.Mutex
	closeCalls  int
	closeSignal chan struct{}
	closeStart  chan struct{}
	deadline    chan time.Time
}

func (p *fakeProvider) captureInternal(InternalObservation) {}

func (p *fakeProvider) close(ctx context.Context) {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	if p.closeStart != nil {
		select {
		case <-p.closeStart:
		default:
			close(p.closeStart)
		}
	}
	if p.deadline != nil {
		if deadline, ok := ctx.Deadline(); ok {
			p.deadline <- deadline
		}
	}
	if p.closeSignal == nil {
		return
	}
	select {
	case <-p.closeSignal:
	case <-ctx.Done():
	}
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls
}

func contextForTest() context.Context { return context.Background() }

func TestQueue_FlushDeadlineIsBounded(t *testing.T) {
	deadline := make(chan time.Time, 1)
	fake := &fakeProvider{deadline: deadline}
	observer := &Observer{closeDone: make(chan struct{}), flushTimeout: 10 * time.Millisecond, providers: []provider{fake}}
	observer.Close(context.Background())
	select {
	case got := <-deadline:
		if remaining := time.Until(got); remaining > 50*time.Millisecond {
			t.Fatalf("close deadline remaining = %s, want bounded", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not receive a deadline")
	}
}

func TestObserver_CloseIsIdempotent(t *testing.T) {
	closeSignal := make(chan struct{})
	closeStart := make(chan struct{})
	fake := &fakeProvider{closeSignal: closeSignal, closeStart: closeStart}
	observer := &Observer{closeDone: make(chan struct{}), providers: []provider{fake}}

	const callers = 8
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			observer.Close(context.Background())
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-closeStart:
	case <-time.After(time.Second):
		t.Fatal("provider close did not start")
	}
	close(closeSignal)
	done.Wait()
	if fake.calls() != 1 {
		t.Fatalf("provider close calls = %d, want 1", fake.calls())
	}
}

func TestObserver_CloseWaitsForInFlightCapture(t *testing.T) {
	fake := &blockingProvider{captureStarted: make(chan struct{}), releaseCapture: make(chan struct{}), closeStarted: make(chan struct{})}
	observer := &Observer{closeDone: make(chan struct{}), providers: []provider{fake}}
	captureDone := make(chan struct{})
	go func() {
		observer.CaptureInternal(context.Background(), validInternalObservationForTest())
		close(captureDone)
	}()
	select {
	case <-fake.captureStarted:
	case <-time.After(time.Second):
		t.Fatal("provider capture did not start")
	}
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	observer.Close(closeCtx)
	select {
	case <-fake.closeStarted:
		t.Fatal("provider close overlapped capture")
	default:
	}
	close(fake.releaseCapture)
	select {
	case <-captureDone:
	case <-time.After(time.Second):
		t.Fatal("provider capture did not finish")
	}
	select {
	case <-fake.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("provider close did not start after capture finished")
	}
}

func TestObserver_ClosedObserverDropsObservations(t *testing.T) {
	fake := &recordingProvider{}
	observer := &Observer{closeDone: make(chan struct{}), providers: []provider{fake}}
	observer.Close(context.Background())
	observer.CaptureInternal(context.Background(), validInternalObservationForTest())
	if fake.internalCalls != 0 {
		t.Fatalf("internal calls = %d, want 0", fake.internalCalls)
	}
}

func TestObserver_ProviderPanicsAreSilent(t *testing.T) {
	observer := &Observer{closeDone: make(chan struct{}), providers: []provider{panickingProvider{}}}
	observer.CaptureInternal(context.Background(), validInternalObservationForTest())
	observer.Close(context.Background())
}

func TestObserver_NonPanicFramesAreRejected(t *testing.T) {
	fake := &recordingProvider{}
	observer := &Observer{closeDone: make(chan struct{}), providers: []provider{fake}}
	observation := validInternalObservationForTest()
	observation.PanicFrames = []StackFrame{{Function: "runtime.function", Lineno: 1}}
	observer.CaptureInternal(context.Background(), observation)
	if fake.internalCalls != 0 {
		t.Fatalf("internal calls = %d, want 0", fake.internalCalls)
	}
}

func TestObserver_ProviderFactoryPanicUsesTelemetryError(t *testing.T) {
	candidate, err := safeProviderFactory(func(Config) (provider, error) {
		panic("secret provider panic")
	}, Config{})
	if candidate != nil {
		t.Fatalf("provider = %T, want nil", candidate)
	}
	if !errors.Is(err, errProviderFactoryPanic) {
		t.Fatalf("factory error = %v, want telemetry provider panic", err)
	}
}

func TestConfig_InvalidSentryAddressSkipsFactory(t *testing.T) {
	calls := 0
	observer := newObserverWithFactory(Config{Enabled: true, SentryDSN: "http://public@example.invalid/1"}, func(Config) (provider, error) {
		calls++
		return &fakeProvider{}, nil
	})
	observer.Close(context.Background())
	if calls != 0 {
		t.Fatalf("provider factory calls = %d, want 0", calls)
	}
}

type recordingProvider struct{ internalCalls int }

func (p *recordingProvider) captureInternal(InternalObservation) { p.internalCalls++ }
func (p *recordingProvider) close(context.Context)               {}

type panickingProvider struct{}

func (panickingProvider) captureInternal(InternalObservation) { panic("capture") }
func (panickingProvider) close(context.Context)               { panic("close") }

type blockingProvider struct {
	captureStarted chan struct{}
	releaseCapture chan struct{}
	closeStarted   chan struct{}
}

func (p *blockingProvider) captureInternal(InternalObservation) {
	close(p.captureStarted)
	<-p.releaseCapture
}

func (p *blockingProvider) close(context.Context) { close(p.closeStarted) }

func validInternalObservationForTest() InternalObservation {
	return InternalObservation{
		Command:         "doctor",
		Stage:           "doctor",
		Code:            "INTERNAL_ERROR",
		RuntimeVersion:  "dev",
		ProtocolVersion: 1,
		Platform:        "windows/amd64",
	}
}
