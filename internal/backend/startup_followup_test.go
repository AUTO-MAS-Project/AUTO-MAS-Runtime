package backend

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type pausedControlMailbox struct {
	*ControlMailbox
	forward chan struct{}
}

func (m *pausedControlMailbox) Receive(ctx context.Context) (protocol.ControlCommand, error) {
	select {
	case <-m.forward:
		return m.ControlMailbox.Receive(ctx)
	case <-ctx.Done():
		return protocol.ControlCommand{}, ctx.Err()
	}
}

type deadlineDiagnosticHealth struct {
	observed chan struct{}
	release  <-chan struct{}
}

func (h deadlineDiagnosticHealth) Check(ctx context.Context, _ health.Expectation, _ health.Probe) error {
	<-ctx.Done()
	if h.observed != nil {
		close(h.observed)
	}
	if h.release != nil {
		<-h.release
	}
	return newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendHealth, "健康检查超时", map[string]any{
		"attemptCount": 3, "durationMs": 23.5, "failureKind": "request_timeout", "state": "running",
	}, ctx.Err())
}

func TestBackend_HealthDeadlineUsesTerminalAcceptanceOrder(t *testing.T) {
	for _, early := range []bool{true, false} {
		t.Run(map[bool]string{true: "stop first", false: "deadline first"}[early], func(t *testing.T) {
			f := newBackendFixture(t)
			f.proc.keepAlive = true
			f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
			healthDone, releaseHealth := make(chan struct{}), make(chan struct{})
			supervisor := f.supervisor()
			supervisor.deps.Health = deadlineDiagnosticHealth{observed: healthDone, release: releaseHealth}
			readyBudget, createBudget := make(chan struct{}), make(chan struct{})
			forward := make(chan struct{})
			control := &pausedControlMailbox{ControlMailbox: NewControlMailbox(8), forward: forward}
			var scopes int
			supervisor.deps.WithStartupTimeout = func(parent context.Context, remaining time.Duration, cause error) (context.Context, context.CancelFunc) {
				scopes++
				if scopes == 2 {
					close(readyBudget)
					<-createBudget
					// 显式区分先后，避免 Windows 时钟精度把两个事件记成同一时刻。
					deadline := time.Now().Add(-time.Second)
					if early {
						accepted, _ := control.TerminalCommand()
						deadline = accepted.AcceptedAt.Add(time.Nanosecond)
					}
					return context.WithDeadlineCause(parent, deadline, cause)
				}
				return context.WithTimeoutCause(parent, remaining, cause)
			}
			done := make(chan error, 1)
			go func() {
				request := f.request()
				request.Control = control
				done <- supervisor.Supervise(t.Context(), request)
			}()
			waitFor(t, readyBudget)
			stop := func() {
				if err := control.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "stop-health-budget"}); err != nil {
					t.Fatal(err)
				}
			}
			if early {
				stop()
			}
			close(createBudget)
			waitFor(t, healthDone)
			if !early {
				stop()
			}
			close(releaseHealth)
			close(forward)
			err := <-done
			if early {
				if err != nil {
					t.Fatalf("accepted stop lost priority: %v", err)
				}
			} else {
				assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
			}
		})
	}
}

func TestBackend_WorkDeadlinePreservesHealthDiagnostics(t *testing.T) {
	for _, phase := range []string{"poll wait", "request"} {
		t.Run(phase, func(t *testing.T) {
			f := newBackendFixture(t)
			f.proc.keepAlive = true
			clock := &pausedHealthClock{startupBudgetClock: &startupBudgetClock{now: time.Unix(0, 0)}, polled: make(chan struct{}, 1)}
			transport := &runningUntilCancelledTransport{entered: make(chan struct{})}
			supervisor := f.supervisor()
			supervisor.deps.Clock = clock.Now
			supervisor.deps.Health = health.NewChecker(health.Config{Clock: clock, Transport: transport})
			expire := make(chan func(), 1)
			var scopes int
			supervisor.deps.WithStartupTimeout = func(parent context.Context, remaining time.Duration, cause error) (context.Context, context.CancelFunc) {
				scopes++
				if scopes == 1 {
					return context.WithTimeoutCause(parent, remaining, cause)
				}
				work, cancel := context.WithCancelCause(parent)
				expire <- func() { cancel(cause) }
				return work, func() { cancel(context.Canceled) }
			}
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				defer close(finished)
				request := f.request()
				request.Control = NewControlMailbox(8)
				done <- supervisor.Supervise(ctx, request)
			}()
			t.Cleanup(func() { cancel(); <-finished })
			// 创建轮询计时器证明第一份合法 running 响应已被 Checker 消费。
			waitFor(t, clock.polled)
			wantAttempts, wantKind := 1, "backend_not_ready"
			if phase == "request" {
				clock.advance(200 * time.Millisecond)
				waitFor(t, transport.entered)
				wantAttempts, wantKind = 2, "request_incomplete"
			} else {
				clock.advance(25 * time.Millisecond)
			}
			(<-expire)()
			err := <-done
			assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
			var failure interface{ Details() map[string]any }
			if !errors.As(err, &failure) {
				t.Fatalf("missing health diagnostics: %v", err)
			}
			details := failure.Details()
			duration, ok := details["durationMs"].(int64)
			if details["attemptCount"] != wantAttempts || details["failureKind"] != wantKind || details["state"] != "running" || !ok || duration <= 0 || duration >= 1000 {
				t.Fatalf("health diagnostics lost: %#v", details)
			}
		})
	}
}

type pausedHealthClock struct {
	*startupBudgetClock
	polled chan struct{}
}

func (c *pausedHealthClock) NewTimer(duration time.Duration) health.Timer {
	c.mu.Lock()
	timer := &startupBudgetTimer{clock: c.startupBudgetClock, deadline: c.now.Add(duration), channel: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	if duration == 200*time.Millisecond {
		c.polled <- struct{}{}
	}
	return timer
}

type runningUntilCancelledTransport struct {
	requests atomic.Int32
	entered  chan struct{}
}

func (r *runningUntilCancelledTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.requests.Add(1) == 1 {
		return (runningHealthTransport{}).RoundTrip(request)
	}
	close(r.entered)
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestBackend_ShutdownDuringHealthDoesNotRetainInducedCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.health.started = make(chan struct{})
	f.health.block = make(chan struct{})
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), request)
	}()
	waitFor(t, f.health.started)
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "stop-during-health"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("graceful shutdown became a failure: %v", err)
	}
}

type cancellableActivation struct {
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	committed atomic.Bool
}

func (a *cancellableActivation) Activate(ctx context.Context, _ int) error {
	close(a.entered)
	select {
	case <-ctx.Done():
		close(a.cancelled)
		return ctx.Err()
	case <-a.release:
		if err := ctx.Err(); err != nil {
			close(a.cancelled)
			return err
		}
		a.committed.Store(true)
		return nil
	}
}
