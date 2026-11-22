//go:build windows

package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func declareDeferredStartup(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, "res"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "res", "runtime-capabilities.json"), []byte(`{"deferredBusinessStart":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredStartup_PreflightDeadlineKeepsRetrySafety(t *testing.T) {
	f := newBackendFixture(t)
	declareDeferredStartup(t, f.layout.RepoDir())
	supervisor := f.supervisor()
	supervisor.deps.WithStartupTimeout = func(parent context.Context, _ time.Duration, cause error) (context.Context, context.CancelFunc) {
		return context.WithTimeoutCause(parent, 0, cause)
	}
	mailbox := NewControlMailbox(8)
	defer mailbox.Close()
	request := f.request()
	request.Control, request.DeferBusinessStart = mailbox, true
	err := supervisor.Supervise(t.Context(), request)
	assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	var failure interface{ Details() map[string]any }
	if !errors.As(err, &failure) || failure.Details()["businessDeferred"] != true {
		t.Fatalf("deferred preflight failure lost safe-retry fact: %v", err)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("spawned %d backends before the preflight deadline", f.uv.startCalls)
	}
}

func TestDeferredStartup_StopCancelsUncommittedActivation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	declareDeferredStartup(t, f.layout.RepoDir())
	activation := &cancellableActivation{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	supervisor := f.supervisor()
	supervisor.deps.Activation = activation
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control, request.DeferBusinessStart = mailbox, true
		done <- supervisor.Supervise(ctx, request)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(ctx, protocol.ControlCommand{Command: protocol.ControlActivate, CommandID: "activate-before-stop"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, activation.entered)
	if err := mailbox.Submit(ctx, protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "stop-before-commit"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-activation.cancelled:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("accepted stop did not cancel in-flight activation")
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	if activation.committed.Load() {
		t.Fatal("activation committed after the accepted stop")
	}
}

type lostActivationAck struct {
	calls   atomic.Int32
	queries atomic.Int32
}

func (a *lostActivationAck) Activate(context.Context, int) error {
	a.calls.Add(1)
	return errors.New("activation acknowledgement lost")
}

func (a *lostActivationAck) Status(context.Context, int) (string, error) {
	a.queries.Add(1)
	return "active", nil
}

func TestDeferredStartup_LostActivationAckQueriesSameBackend(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	declareDeferredStartup(t, f.layout.RepoDir())
	mailbox := NewControlMailbox(8)
	activator := &lostActivationAck{}
	supervisor := f.supervisor()
	supervisor.deps.Activation = activator
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control, request.DeferBusinessStart = mailbox, true
		done <- supervisor.Supervise(t.Context(), request)
	}()
	waitFor(t, f.emitter.running)
	for _, command := range []protocol.ControlCommand{
		{Command: protocol.ControlActivate, CommandID: "lost-activate"},
		{Command: protocol.ControlStatus, CommandID: "query-activation"},
	} {
		if err := mailbox.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		waitForStatusCommand(t, f.emitter, command.CommandID)
	}
	if activator.calls.Load() != 1 || activator.queries.Load() != 1 {
		t.Fatalf("activation/query = %d/%d, want 1/1", activator.calls.Load(), activator.queries.Load())
	}
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	assertBackendCode(t, <-done, protocol.CodeOperationCancelled)
	if f.uv.startCalls != 1 {
		t.Fatalf("spawned %d backends after lost acknowledgement", f.uv.startCalls)
	}
}

func TestDeferredStartup_ActivatedBackendRestartActivatesSuccessorOnce(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	second := &fakeProcess{pid: 4343, keepAlive: true}
	f.uv.procSequence = []ManagedProcess{f.proc, second}
	declareDeferredStartup(t, f.layout.RepoDir())
	mailbox := NewControlMailbox(8)
	activator := &recordingActivator{}
	supervisor := f.supervisor()
	supervisor.deps.Activation = activator
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control, request.DeferBusinessStart = mailbox, true
		done <- supervisor.Supervise(t.Context(), request)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlActivate, CommandID: "activate"}); err != nil {
		t.Fatal(err)
	}
	waitForStatusCommand(t, f.emitter, "activate")
	f.proc.Exit()
	waitForStateStatusCount(t, f.emitter, protocol.StateRunning, 4)
	if activator.calls.Load() != 2 {
		t.Fatalf("activated %d times across two backends", activator.calls.Load())
	}
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	assertBackendCode(t, <-done, protocol.CodeOperationCancelled)
	if f.uv.startCalls != 2 {
		t.Fatalf("spawned %d backends, want one successor", f.uv.startCalls)
	}
}

func TestDeferredStartup_CapabilityMustBeDeclaredBeforeSpawn(t *testing.T) {
	repo := t.TempDir()
	if err := checkDeferredStartupCapability(context.Background(), repo); err == nil {
		t.Fatal("missing capability was accepted")
	}
	if err := os.Mkdir(filepath.Join(repo, "res"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "res", "runtime-capabilities.json"), []byte(`{"deferredBusinessStart":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDeferredStartupCapability(context.Background(), repo); err != nil {
		t.Fatalf("declared capability rejected: %v", err)
	}
}

type recordingActivator struct{ calls atomic.Int32 }

func (a *recordingActivator) Activate(context.Context, int) error { a.calls.Add(1); return nil }

func TestDeferredStartup_ActivationIsIdempotent(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	if err := os.MkdirAll(filepath.Join(f.layout.RepoDir(), "res"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.layout.RepoDir(), "res", "runtime-capabilities.json"), []byte(`{"deferredBusinessStart":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mailbox := NewControlMailbox(8)
	activator := &recordingActivator{}
	supervisor := f.supervisor()
	supervisor.deps.Activation = activator
	checking := make(chan struct{})
	supervisor.deps.VerifyPendingSource = func(ctx context.Context, _ string) (bool, error) {
		if activator.calls.Load() != 1 {
			t.Error("source inspection started before business activation")
		}
		close(checking)
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.DeferBusinessStart = true
		done <- supervisor.Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	if activator.calls.Load() != 0 {
		t.Fatal("activated before host confirmation")
	}
	for _, id := range []string{"activate-first", "activate-again"} {
		if err := mailbox.Submit(ctx, protocol.ControlCommand{Command: protocol.ControlActivate, CommandID: id}); err != nil {
			t.Fatal(err)
		}
		waitForStatusCommand(t, f.emitter, id)
	}
	if activator.calls.Load() != 1 {
		t.Fatalf("activation calls = %d", activator.calls.Load())
	}
	waitFor(t, checking)
	if !f.uv.options.DeferBusinessStart {
		t.Fatal("deferred start was not passed to the child boundary")
	}
	if err := mailbox.Submit(ctx, protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !hasBackendCode(err, protocol.CodeOperationCancelled) {
		t.Fatalf("cancel failed: %v", err)
	}
}

func TestDeferredStartup_ExitBeforeActivationDoesNotRestart(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	if err := os.MkdirAll(filepath.Join(f.layout.RepoDir(), "res"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.layout.RepoDir(), "res", "runtime-capabilities.json"), []byte(`{"deferredBusinessStart":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.DeferBusinessStart = true
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeBackendExitedBeforeReady)
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("pending startup restarted or failed to settle")
	}
	if f.uv.startCalls != 1 {
		t.Fatalf("started %d processes before activation", f.uv.startCalls)
	}
}
