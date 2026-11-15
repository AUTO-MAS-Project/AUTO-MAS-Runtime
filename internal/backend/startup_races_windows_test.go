//go:build windows

package backend

import (
	"context"
	"errors"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeferredStartup_LegacyPreferenceDoesNotClaimDeferredBusiness(t *testing.T) {
	f := newBackendFixture(t)
	f.health.err = newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendHealth, "health timeout", nil, nil)
	mailbox := NewControlMailbox(8)
	defer mailbox.Close()
	request := f.request()
	request.Control = mailbox
	request.DeferBusinessStart = true
	request.AllowLegacyBusinessStart = true
	err := f.supervisor().Supervise(t.Context(), request)
	assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("missing structured failure: %v", err)
	}
	if f.uv.options.DeferBusinessStart || failure.Details()["businessDeferred"] == true {
		t.Fatal("legacy backend was marked as safe to retry automatically")
	}
}

func TestDeferredStartup_CapabilityIsCheckedUnderLock(t *testing.T) {
	f := newBackendFixture(t)
	declareDeferredStartup(t, f.layout.RepoDir())
	f.lock.acquireStarted = make(chan struct{})
	f.lock.acquireBlock = make(chan struct{})
	f.health.err = newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendHealth, "health timeout", nil, nil)
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = mailbox
		request.DeferBusinessStart = true
		done <- f.supervisor().Supervise(t.Context(), request)
	}()
	waitFor(t, f.lock.acquireStarted)
	if err := os.Remove(config.BackendCapabilitiesFile(f.layout.RepoDir())); err != nil {
		t.Fatal(err)
	}
	close(f.lock.acquireBlock)
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeUnsupportedMode)
		t.Logf("spawned after declaration was removed = %d, deferred option = %t", f.uv.startCalls, f.uv.options.DeferBusinessStart)
		if f.uv.startCalls != 0 {
			t.Fatal("backend spawned without a capability declaration under lock")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervision did not settle")
	}
}

type delayedPreflightUV struct {
	*fakeUV
	started chan struct{}
	expired chan struct{}
	release chan struct{}
}

func (u *delayedPreflightUV) Check(ctx context.Context, options uv.RunOptions) error {
	if u.started != nil {
		close(u.started)
	}
	<-ctx.Done()
	close(u.expired)
	<-u.release
	return ctx.Err()
}

func TestBackend_EarlyShutdownDoesNotBecomeTimeoutDuringPreflightCleanup(t *testing.T) {
	f := newBackendFixture(t)
	mailbox := NewControlMailbox(8)
	slow := &delayedPreflightUV{fakeUV: f.uv, started: make(chan struct{}), expired: make(chan struct{}), release: make(chan struct{})}
	supervisor := f.supervisor()
	supervisor.deps.UV = slow
	workScope := make(chan context.Context, 1)
	supervisor.deps.WithStartupTimeout = func(parent context.Context, remaining time.Duration, cause error) (context.Context, context.CancelFunc) {
		work, cancel := context.WithTimeoutCause(parent, remaining, cause)
		workScope <- work
		return work, cancel
	}
	var calls atomic.Int32
	origin := time.Unix(0, 0)
	supervisor.deps.Clock = func() time.Time {
		if calls.Add(1) == 1 {
			return origin
		}
		return origin.Add(startupWorkBudget - time.Second)
	}
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = mailbox
		done <- supervisor.Supervise(t.Context(), request)
	}()
	waitFor(t, slow.started)
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "early-preflight-shutdown"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, slow.expired)
	<-((<-workScope).Done())
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatalf("early shutdown changed into a timeout: %v", err)
	}
}

func TestBackend_PreflightTimeoutSurvivesLateShutdown(t *testing.T) {
	f := newBackendFixture(t)
	mailbox := NewControlMailbox(8)
	slow := &delayedPreflightUV{fakeUV: f.uv, expired: make(chan struct{}), release: make(chan struct{})}
	supervisor := f.supervisor()
	supervisor.deps.UV = slow
	var calls atomic.Int32
	origin := time.Unix(0, 0)
	supervisor.deps.Clock = func() time.Time {
		if calls.Add(1) == 1 {
			return origin
		}
		return origin.Add(startupWorkBudget - 10*time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = mailbox
		done <- supervisor.Supervise(t.Context(), request)
	}()
	waitFor(t, slow.expired)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "late-preflight-shutdown"}); err != nil {
		t.Fatal(err)
	}
	close(slow.release)
	select {
	case err := <-done:
		t.Logf("result after preflight deadline expired before shutdown = %v", err)
		assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("supervision did not settle")
	}
}

func TestBackend_EarlyAcceptedShutdownSurvivesDelayedForwarding(t *testing.T) {
	f := newBackendFixture(t)
	slow := &delayedPreflightUV{fakeUV: f.uv, started: make(chan struct{}), expired: make(chan struct{}), release: make(chan struct{})}
	control := &pausedControlMailbox{ControlMailbox: NewControlMailbox(8), forward: make(chan struct{})}
	supervisor := f.supervisor()
	supervisor.deps.UV = slow
	var clockCalls atomic.Int32
	origin := time.Unix(0, 0)
	supervisor.deps.Clock = func() time.Time {
		if clockCalls.Add(1) == 1 {
			return origin
		}
		return origin.Add(startupWorkBudget - 200*time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = control
		done <- supervisor.Supervise(t.Context(), request)
	}()
	waitFor(t, slow.started)
	select {
	case <-slow.expired:
		t.Fatal("deadline expired before early command could be accepted")
	default:
	}
	if err := control.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "early-accepted-shutdown"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, slow.expired)
	close(slow.release)
	close(control.forward)
	select {
	case err := <-done:
		t.Logf("accepted shutdown before deadline, delivered after deadline, result=%v", err)
		if err != nil {
			t.Fatalf("accepted shutdown was replaced by timeout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not settle")
	}
}
