package backend

import (
	"context"
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
	f := newBackendFixture(t)
	supervisor := f.supervisor()
	supervisor.deps.Clock = time.Now
	supervisor.deps.Health = deadlineDiagnosticHealth{}
	request := f.request()
	request.startupDeadline = time.Now().Add(time.Millisecond)
	err := supervisor.checkStartupHealth(t.Context(), request, health.Expectation{}, nil)
	assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	failure := err.(interface{ Details() map[string]any })
	details := failure.Details()
	if details["attemptCount"] != 3 || details["durationMs"] != 23.5 || details["failureKind"] != "request_timeout" || details["state"] != "running" {
		t.Fatalf("health diagnostics lost: %#v", details)
	}
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
