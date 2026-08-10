package backend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestBackend_ControlMailboxNeverDropsTerminalOrStatusCommands(t *testing.T) {
	mailbox := NewControlMailbox(1)
	status := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-1"}
	shutdown := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-1"}
	if err := mailbox.Submit(context.Background(), status); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	terminalDone := make(chan error, 1)
	go func() { terminalDone <- mailbox.Submit(context.Background(), shutdown) }()
	select {
	case <-mailbox.fullWaiters:
	case <-time.After(time.Second):
		t.Fatal("terminal submit did not observe full mailbox")
	}
	gotStatus, err := mailbox.Receive(context.Background())
	if err != nil || gotStatus != status {
		t.Fatalf("Receive(status) = %#v, %v, want %#v", gotStatus, err, status)
	}
	if err := <-terminalDone; err != nil {
		t.Fatalf("Submit(full terminal) error = %v", err)
	}
	if _, ok := mailbox.TerminalCommand(); !ok {
		t.Fatal("successful terminal submit did not set terminal latch")
	}
	gotShutdown, err := mailbox.Receive(context.Background())
	if err != nil || gotShutdown != shutdown {
		t.Fatalf("Receive(shutdown) = %#v, %v, want %#v", gotShutdown, err, shutdown)
	}
	mailbox.StopAccepting()
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "after-stop"}); !errors.Is(err, ErrControlStopped) {
		t.Fatalf("Submit(after StopAccepting) error = %v, want ErrControlStopped", err)
	}
}

func TestBackend_ControlMailbox_StopWakesFullProducer(t *testing.T) {
	mailbox := NewControlMailbox(1)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-full"}); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-full"})
	}()
	select {
	case <-mailbox.fullWaiters:
	case <-time.After(time.Second):
		t.Fatal("full producer did not block on capacity")
	}
	mailbox.StopAccepting()
	select {
	case err := <-done:
		if !errors.Is(err, ErrControlStopped) {
			t.Fatalf("Submit(full) error = %v, want ErrControlStopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopAccepting did not wake full producer")
	}
}

func TestBackend_ControlMailbox_CancelFirstKeepsShutdownAndStatusRoutable(t *testing.T) {
	mailbox := NewControlMailbox(4)
	commands := []protocol.ControlCommand{
		{Protocol: protocol.Version, Command: protocol.ControlCancel, CommandID: "cancel-first"},
		{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-second"},
		{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-after"},
	}
	for _, command := range commands {
		if err := mailbox.Submit(context.Background(), command); err != nil {
			t.Fatalf("Submit(%s) error = %v", command.Command, err)
		}
	}
	for _, want := range commands {
		got, err := mailbox.Receive(context.Background())
		if err != nil || got != want {
			t.Fatalf("Receive() = %#v, %v, want %#v", got, err, want)
		}
	}
	terminal, ok := mailbox.TerminalCommand()
	if !ok || terminal.CommandID != commands[0].CommandID {
		t.Fatalf("TerminalCommand() = %#v/%t, want first cancel", terminal, ok)
	}
}

func TestBackend_ActiveShutdownOrCancelDoesNotRestart(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.health.started = make(chan struct{})
	f.health.block = make(chan struct{})
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.health.started)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-1"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !hasBackendCode(err, protocol.CodeOperationCancelled) {
			t.Fatalf("Supervise() error = %v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("cancel was not consumed while health check was blocked")
	}
	if f.uv.startCalls != 1 {
		t.Fatalf("StartManaged calls = %d, want 1", f.uv.startCalls)
	}
}

func TestBackend_CancelDrainsStatusDuringCleanup(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.cleanupStarted = make(chan struct{})
	f.proc.cleanupRelease = make(chan struct{})
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-cleanup"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	waitFor(t, f.proc.cleanupStarted)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-cleanup"}); err != nil {
		t.Fatalf("Submit(status) during cleanup error = %v", err)
	}
	waitForStatusCommand(t, f.emitter, "status-cleanup")
	close(f.proc.cleanupRelease)
	if err := <-done; !errors.Is(err, context.Canceled) && !hasBackendCode(err, protocol.CodeOperationCancelled) {
		t.Fatalf("Supervise() error = %v, want cancellation", err)
	}
}

func TestBackend_ExitCleanupHonorsPendingCancelBeforeRestart(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.cleanupStarted = make(chan struct{})
	f.proc.cleanupRelease = make(chan struct{})
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	waitFor(t, f.proc.cleanupStarted)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-during-exit-cleanup"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-during-exit-cleanup"}); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	close(f.proc.cleanupRelease)
	err := <-done
	if !errors.Is(err, context.Canceled) && !hasBackendCode(err, protocol.CodeOperationCancelled) {
		t.Fatalf("Supervise() error = %v, want cancellation", err)
	}
	if f.uv.startCalls != 1 {
		t.Fatalf("StartManaged calls = %d, want 1", f.uv.startCalls)
	}
	if indexOfEvent(f.emitter.eventsSnapshot(), "state:"+string(protocol.StateRestarting)) >= 0 {
		t.Fatal("pending cancel did not suppress restart")
	}
	waitForStatusCommand(t, f.emitter, "status-during-exit-cleanup")
}

func TestBackend_CancelCleanupReaderErrorDoesNotDeadlock(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.cleanupStarted = make(chan struct{})
	f.proc.cleanupRelease = make(chan struct{})
	mailbox := NewControlMailbox(8)
	ctx := t.Context()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	cancel := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlCancel, CommandID: "cancel-reader-error"}
	if err := mailbox.Submit(context.Background(), cancel); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	waitFor(t, f.proc.cleanupStarted)
	mailbox.SetReaderError(errors.New("control reader failed during cleanup"))
	close(f.proc.cleanupRelease)
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeInternalError)
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise() did not finish after cleanup reader error")
	}
	states := f.emitter.states()
	if len(states) == 0 || states[len(states)-1].Status != protocol.StateBackendFailed {
		t.Fatalf("states = %#v, want backend_failed after reader error", states)
	}
}

func TestBackend_StatusIsReadOnlyAndEchoesCommandID(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	updatesBefore := f.state.updateCalls
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-1"}); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	waitForStatusCommand(t, f.emitter, "status-1")
	if got := f.state.updateCalls; got != updatesBefore {
		t.Fatalf("state update calls = %d, want read-only %d", got, updatesBefore)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
}

func TestBackend_ControlReaderJoinAndReadFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	release := make(chan struct{})
	receiver := &failingControlReceiver{err: errors.New("reader failed"), ready: make(chan struct{}), release: release}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := f.request()
	req.Control = receiver
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, req) }()
	waitFor(t, f.emitter.running)
	close(release)
	err := <-done
	assertBackendCode(t, err, protocol.CodeInternalError)
	if !f.proc.terminated || !f.proc.waitedEmpty || !f.proc.closed || f.logger.closeCalls != 1 {
		t.Fatalf("reader failure cleanup = process(%v,%v,%v) logger=%d", f.proc.terminated, f.proc.waitedEmpty, f.proc.closed, f.logger.closeCalls)
	}
}

func TestBackend_HealthFailureDrainsAcceptedStatusBeforeFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.health.err = newError(protocol.CodeBackendHealthInvalid, protocol.StageBackendHealth, "后端健康响应无效", nil, errors.New("invalid health"))
	f.health.started = make(chan struct{})
	healthRelease := make(chan struct{})
	f.health.block = healthRelease
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.health.started)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-health-failure"}); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	close(healthRelease)
	assertBackendCode(t, <-done, protocol.CodeBackendHealthInvalid)
	states := f.emitter.states()
	statusIndex, failureIndex := -1, -1
	for index, event := range states {
		if event.Details["controlCommandId"] == "status-health-failure" {
			statusIndex = index
		}
		if event.Status == protocol.StateBackendFailed {
			failureIndex = index
		}
	}
	if statusIndex < 0 || failureIndex < 0 || statusIndex >= failureIndex {
		t.Fatalf("states = %#v, want accepted status before backend_failed", states)
	}
}

func TestBackend_ReaderErrorDrainsQueuedStatusBeforeFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlStatus, CommandID: "status-reader-error"}); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	mailbox.SetReaderError(errors.New("reader failed"))
	assertBackendCode(t, <-done, protocol.CodeInternalError)
	states := f.emitter.states()
	statusIndex, failureIndex := -1, -1
	for index, event := range states {
		if event.Details["controlCommandId"] == "status-reader-error" {
			statusIndex = index
		}
		if event.Status == protocol.StateBackendFailed {
			failureIndex = index
		}
	}
	if statusIndex < 0 || failureIndex < 0 || statusIndex >= failureIndex {
		t.Fatalf("states = %#v, want queued status before backend_failed", states)
	}
}

func TestBackend_NonNilSpawnErrorPreservesPendingOutputFault(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.startErr = errors.New("spawn failed after process creation")
	f.uv.returnProcOnErr = true
	f.proc.startRecords = make([]process.StreamRecord, maxPendingLogEvents+1)
	for index := range f.proc.startRecords {
		f.proc.startRecords[index] = process.StreamRecord{Stream: process.StreamStdout, Event: "boot", EndOfLine: true}
	}
	mailbox := NewControlMailbox(8)
	req := f.request()
	req.Control = mailbox
	err := f.supervisor().Supervise(t.Context(), req)
	assertBackendCode(t, err, protocol.CodeOutputWriteFailed)
	if !f.proc.terminated || !f.proc.waitedEmpty || !f.proc.closed || f.logger.closeCalls != 1 {
		t.Fatalf("spawn fault cleanup = process(%v,%v,%v) logger=%d", f.proc.terminated, f.proc.waitedEmpty, f.proc.closed, f.logger.closeCalls)
	}
}

func TestBackend_ControlInfrastructureOutranksContextCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	mailbox.SetReaderError(errors.New("reader failed"))
	cancel()
	err := <-done
	assertBackendCode(t, err, protocol.CodeInternalError)
	states := f.emitter.states()
	if len(states) == 0 || states[len(states)-1].Status != protocol.StateBackendFailed {
		t.Fatalf("states = %#v, want backend_failed after reader error", states)
	}
}

func TestBackend_GateFaultOutranksContextCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	releaseRunning := make(chan struct{})
	f.emitter.runningRelease = releaseRunning
	f.emitter.logErr = errors.New("protocol sink failed")
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	if err := f.proc.EmitRecord(context.Background(), process.StreamRecord{Stream: process.StreamStdout, Event: "fault", EndOfLine: true}); err == nil {
		t.Fatal("EmitRecord() error = nil, want protocol sink fault")
	}
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlCancel, CommandID: "cancel-exit-gate"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	f.proc.Exit()
	cancel()
	close(releaseRunning)
	err := <-done
	assertBackendCode(t, err, protocol.CodeOutputWriteFailed)
}

func TestBackend_UpdateErrorGateAndContextPriority(t *testing.T) {
	for _, stage := range []protocol.Stage{protocol.StageBackendHealth, protocol.StageBackendRun} {
		t.Run(string(stage), func(t *testing.T) {
			f := newBackendFixture(t)
			f.proc.keepAlive = true
			f.state.updateErrStage = stage
			f.state.updateErr = errors.New("state update failed")
			f.state.updateStarted = make(chan struct{})
			f.state.updateBlock = make(chan struct{})
			f.emitter.logErr = errors.New("protocol sink failed")
			mailbox := NewControlMailbox(8)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				req := f.request()
				req.Control = mailbox
				done <- f.supervisor().Supervise(ctx, req)
			}()
			waitFor(t, f.state.updateStarted)
			if err := f.proc.EmitRecord(context.Background(), process.StreamRecord{Stream: process.StreamStdout, Event: "fault", EndOfLine: true}); err == nil {
				t.Fatal("EmitRecord() error = nil, want protocol sink fault")
			}
			cancel()
			close(f.state.updateBlock)
			assertBackendCode(t, <-done, protocol.CodeOutputWriteFailed)
		})
	}
}

func TestBackend_AcceptedShutdownOutranksContextCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.health.started = make(chan struct{})
	f.health.block = make(chan struct{})
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = control
		done <- f.supervisorWithHTTP(ctx, req)
	}()
	waitFor(t, f.health.started)
	command := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-cancel-race"}
	if err := mailbox.Submit(context.Background(), command); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	cancel()
	close(releaseReceive)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want successful shutdown", err)
	}
	states := f.emitter.states()
	var stopped *protocol.StateEvent
	for index := range states {
		if states[index].Status == protocol.StateStopped {
			stopped = &states[index]
		}
	}
	if stopped == nil {
		t.Fatalf("states = %#v, want stopped state", states)
	}
	if got := stopped.Details["controlCommandId"]; got != command.CommandID {
		t.Fatalf("stopped controlCommandId = %v, want %q", got, command.CommandID)
	}
}

func TestBackend_PreflightAcceptedShutdownOutranksContextCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.started = make(chan struct{})
	f.repository.block = make(chan struct{})
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	commandID := "shutdown-preflight-cancel-race"
	var shutdownID string
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeShutdown = func(id string) { shutdownID = id }
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.repository.started)
	command := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: commandID}
	if err := mailbox.Submit(context.Background(), command); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	cancel()
	close(releaseReceive)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want successful preflight shutdown", err)
	}
	if shutdownID != commandID {
		t.Fatalf("BeforeShutdown commandID = %q, want %q", shutdownID, commandID)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_PreflightDoneCancellationDoesNotSpawn(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.blockOnCall = 1
	f.repository.block = make(chan struct{})
	f.repository.blockStarted = make(chan struct{})
	mailbox := NewControlMailbox(8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.repository.blockStarted)
	cancel()
	close(f.repository.block)
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_PreflightCancelDrainsAcceptedStatusBeforeCancelled(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.started = make(chan struct{})
	f.repository.block = make(chan struct{})
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.repository.started)
	cancelCommand := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlCancel, CommandID: "cancel-preflight"}
	statusCommand := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-preflight"}
	if err := mailbox.Submit(context.Background(), cancelCommand); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	if err := mailbox.Submit(context.Background(), statusCommand); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	close(releaseReceive)
	err := <-done
	assertBackendCode(t, err, protocol.CodeOperationCancelled)
	var details interface{ Details() map[string]any }
	if !errors.As(err, &details) || details.Details()["controlCommandId"] != cancelCommand.CommandID {
		t.Fatalf("cancel error details = %#v, want command ID %q", details, cancelCommand.CommandID)
	}
	waitForStatusCommand(t, f.emitter, statusCommand.CommandID)
	for _, event := range f.emitter.states() {
		if event.Status == protocol.StateStartingBackend || event.Status == protocol.StateStoppingBackend || event.Status == protocol.StateStopped {
			t.Fatalf("states = %#v, want no lifecycle state for preflight cancellation", f.emitter.states())
		}
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_PreflightStatusOutputFailureOutranksBusinessError(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.block = make(chan struct{})
	f.repository.blockOnCall = 1
	f.repository.blockStarted = make(chan struct{})
	f.repository.errOnCall = 1
	f.repository.err = errors.New("repository unavailable")
	f.emitter.stateErr = newError(protocol.CodeOutputWriteFailed, protocol.StageBackendSpawn, "status output failed", map[string]any{"sink": "protocol_output"}, errors.New("sink failed"))
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.repository.blockStarted)
	status := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-preflight-output-failure"}
	if err := mailbox.Submit(context.Background(), status); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	shutdown := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-preflight-output-failure"}
	if err := mailbox.Submit(context.Background(), shutdown); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	close(f.repository.block)
	close(releaseReceive)
	assertBackendCode(t, <-done, protocol.CodeOutputWriteFailed)
}

func TestBackend_PreflightDrainReaderErrorMapsInternal(t *testing.T) {
	f := newBackendFixture(t)
	results := make(chan controlResult, 1)
	results <- controlResult{err: errors.New("reader failed after preflight error")}
	snapshot := &controlState{stage: protocol.StageBackendSpawn, status: protocol.StateReadyToStart, controlResults: results}
	req := f.request()
	err := f.supervisor().closeControlBeforeFailure(req, snapshot, false)
	assertBackendCode(t, err, protocol.CodeInternalError)
	var coded interface{ Stage() protocol.Stage }
	if !errors.As(err, &coded) || coded.Stage() != protocol.StageBackendSpawn {
		t.Fatalf("error stage = %#v, want %s", err, protocol.StageBackendSpawn)
	}
}

func TestBackend_PreflightReaderErrorOutranksAcceptedShutdown(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.block = make(chan struct{})
	f.repository.blockOnCall = 1
	f.repository.blockStarted = make(chan struct{})
	f.repository.errOnCall = 1
	f.repository.err = errors.New("repository unavailable")
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.repository.blockStarted)
	mailbox.SetReaderError(errors.New("reader failed before preflight completed"))
	shutdown := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-preflight-reader-error"}
	if err := mailbox.Submit(context.Background(), shutdown); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	close(f.repository.block)
	err := <-done
	assertBackendCode(t, err, protocol.CodeInternalError)
	var coded interface{ Stage() protocol.Stage }
	if !errors.As(err, &coded) || coded.Stage() != protocol.StageBackendSpawn {
		t.Fatalf("error stage = %#v, want %s", err, protocol.StageBackendSpawn)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_LockAcquireFailureDrainsAcceptedStatusBeforeFailure(t *testing.T) {
	f := newBackendFixture(t)
	f.lock.acquireErr = errors.New("mutex busy")
	f.lock.acquireStarted = make(chan struct{})
	f.lock.acquireBlock = make(chan struct{})
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.lock.acquireStarted)
	status := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-lock-failure"}
	if err := mailbox.Submit(context.Background(), status); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	close(f.lock.acquireBlock)
	close(releaseReceive)
	assertBackendCode(t, <-done, protocol.CodeMutexOperationFailed)
	states := f.emitter.states()
	statusIndex := -1
	for index, event := range states {
		if event.Details["controlCommandId"] == status.CommandID {
			statusIndex = index
		}
	}
	if statusIndex < 0 {
		t.Fatalf("states = %#v, want accepted status before lock failure return", states)
	}
}

func TestBackend_LockAcquireFailurePreservesAcceptedShutdown(t *testing.T) {
	f := newBackendFixture(t)
	f.lock.acquireErr = errors.New("mutex busy")
	f.lock.acquireStarted = make(chan struct{})
	f.lock.acquireBlock = make(chan struct{})
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeShutdown = mailbox.BeforeShutdown
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.lock.acquireStarted)
	status := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-lock-shutdown"}
	shutdown := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-lock-failure"}
	if err := mailbox.Submit(context.Background(), status); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	if err := mailbox.Submit(context.Background(), shutdown); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	close(f.lock.acquireBlock)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil for accepted shutdown", err)
	}
	states := f.emitter.states()
	statusIndex := -1
	for index, event := range states {
		if event.Details["controlCommandId"] == status.CommandID {
			statusIndex = index
		}
		if event.Status == protocol.StateStartingBackend || event.Status == protocol.StateStoppingBackend || event.Status == protocol.StateStopped {
			t.Fatalf("states = %#v, want no lifecycle state before spawn", states)
		}
	}
	if statusIndex < 0 {
		t.Fatalf("states = %#v, want accepted status before shutdown result", states)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_StartPreludeCancelDrainsAcceptedStatusBeforeCancelled(t *testing.T) {
	f := newBackendFixture(t)
	f.state.beginStarted = make(chan struct{})
	f.state.beginBlock = make(chan struct{})
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.state.beginStarted)
	cancelCommand := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlCancel, CommandID: "cancel-start-prelude"}
	statusCommand := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlStatus, CommandID: "status-start-prelude"}
	if err := mailbox.Submit(context.Background(), cancelCommand); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	if err := mailbox.Submit(context.Background(), statusCommand); err != nil {
		t.Fatalf("Submit(status) error = %v", err)
	}
	close(f.state.beginBlock)
	close(releaseReceive)
	err := <-done
	assertBackendCode(t, err, protocol.CodeOperationCancelled)
	waitForStatusCommand(t, f.emitter, statusCommand.CommandID)
	for _, event := range f.emitter.states() {
		if event.Status == protocol.StateStartingBackend || event.Status == protocol.StateStoppingBackend || event.Status == protocol.StateStopped {
			t.Fatalf("states = %#v, want no lifecycle state for pre-spawn cancellation", f.emitter.states())
		}
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_StartErrorPreservesAcceptedShutdownDuringCancellation(t *testing.T) {
	f := newBackendFixture(t)
	f.repository.blockOnCall = 2
	f.repository.block = make(chan struct{})
	f.repository.blockStarted = make(chan struct{})
	mailbox := NewControlMailbox(8)
	releaseReceive := make(chan struct{})
	control := &delayedControlReceiver{receiver: mailbox, release: releaseReceive}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var shutdownID string
	go func() {
		req := f.request()
		req.Control = control
		req.BeforeShutdown = func(id string) { shutdownID = id }
		done <- f.supervisor().Supervise(ctx, req)
	}()
	waitFor(t, f.repository.blockStarted)
	command := protocol.ControlCommand{Protocol: protocol.Version, Command: protocol.ControlShutdown, CommandID: "shutdown-start-error"}
	if err := mailbox.Submit(context.Background(), command); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	cancel()
	close(releaseReceive)
	close(f.repository.block)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want accepted shutdown success", err)
	}
	if shutdownID != command.CommandID {
		t.Fatalf("BeforeShutdown commandID = %q, want %q", shutdownID, command.CommandID)
	}
	if f.uv.startCalls != 0 {
		t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
	}
}

func TestBackend_ShutdownStopsReaderBeforeHTTP(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	var mu sync.Mutex
	order := make([]string, 0, 2)
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeShutdown = func(string) {
			mu.Lock()
			order = append(order, "reader")
			mu.Unlock()
		}
		done <- f.supervisorWithHTTP(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-1"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "reader" || order[1] != "http" {
		t.Fatalf("shutdown order = %#v, want reader then http", order)
	}
}

func TestBackend_GracefulShutdown(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	ctx := t.Context()
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisorWithHTTP(ctx, req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-graceful"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	for _, event := range f.emitter.eventsSnapshot() {
		if event == "warning:BACKEND_FORCE_TERMINATED" {
			t.Fatal("graceful shutdown emitted force warning")
		}
	}
}

func TestBackend_GracefulShutdownForceClearsDescendants(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.waitErrs = []error{context.DeadlineExceeded, nil}
	f.proc.waitEmptyErrs = []error{context.DeadlineExceeded, nil}
	mailbox := NewControlMailbox(8)
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisorWithHTTP(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-descendants"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil after forced empty tree", err)
	}
	if indexOfEvent(f.emitter.eventsSnapshot(), "warning:"+string(protocol.CodeBackendForceTerminated)) < 0 {
		t.Fatalf("events = %#v, want force warning", f.emitter.eventsSnapshot())
	}
}

func TestBackend_ForceTerminationWarnsAndSucceeds(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.shutdownTimeout = 10 * time.Millisecond
	f.depsHTTP = errorHTTPCloser{}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisorWithHTTP(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-force"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	if indexOfEvent(f.emitter.eventsSnapshot(), "warning:BACKEND_FORCE_TERMINATED") < 0 {
		t.Fatalf("events = %#v, want force warning", f.emitter.eventsSnapshot())
	}
}

func TestBackend_ShutdownFailedIfTreeUncertain(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.waitEmptyErr = errors.New("job still contains a descendant")
	f.shutdownTimeout = 10 * time.Millisecond
	f.depsHTTP = errorHTTPCloser{}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisorWithHTTP(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-uncertain"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	assertBackendCode(t, <-done, protocol.CodeBackendShutdownFailed)
	if indexOfEvent(f.emitter.eventsSnapshot(), "warning:BACKEND_FORCE_TERMINATED") >= 0 {
		t.Fatal("uncertain tree emitted force warning")
	}
}

func TestBackend_FirstUnexpectedExitRestartsOnce(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	second := &fakeProcess{pid: 4343, keepAlive: true}
	f.uv.procSequence = []ManagedProcess{f.proc, second}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	waitForStateStatusCount(t, f.emitter, protocol.StateRunning, 2)
	if f.uv.startCalls != 2 {
		t.Fatalf("StartManaged calls = %d, want 2", f.uv.startCalls)
	}
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-after-restart"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	if err := <-done; !hasBackendCode(err, protocol.CodeOperationCancelled) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want cancellation", err)
	}
}

func TestBackend_RestartRechecksEnvironment(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.state.secondEnvironment = &state.EnvironmentState{Status: protocol.StateEnvironmentBroken, LastSuccessful: f.state.environment.LastSuccessful}
	f.state.secondEnvironmentAfter = 3
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	assertBackendCode(t, <-done, protocol.CodeBackendRestartFailed)
	if f.uv.startCalls != 1 {
		t.Fatalf("StartManaged calls = %d, want 1", f.uv.startCalls)
	}
}

func TestBackend_RestartOutputFaultPreservesCode(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	second := &fakeProcess{pid: 4343, keepAlive: true, startRecords: make([]process.StreamRecord, maxPendingLogEvents+1)}
	for index := range second.startRecords {
		second.startRecords[index] = process.StreamRecord{Stream: process.StreamStdout, Event: "boot", EndOfLine: true}
	}
	f.uv.procSequence = []ManagedProcess{f.proc, second}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	err := <-done
	assertBackendCode(t, err, protocol.CodeOutputWriteFailed)
	if f.uv.startCalls != 2 {
		t.Fatalf("StartManaged calls = %d, want 2", f.uv.startCalls)
	}
}

func TestBackend_SecondUnexpectedExitStopsWithoutThirdSpawn(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	second := &fakeProcess{pid: 4343, keepAlive: true}
	f.uv.procSequence = []ManagedProcess{f.proc, second}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	waitForStateStatusCount(t, f.emitter, protocol.StateRunning, 2)
	second.Exit()
	assertBackendCode(t, <-done, protocol.CodeBackendExitedUnexpectedly)
	if f.uv.startCalls != 2 {
		t.Fatalf("StartManaged calls = %d, want 2", f.uv.startCalls)
	}
}

func TestBackend_ActiveShutdownOrCancelDoesNotRestartAfterRunning(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	releaseRunning := make(chan struct{})
	f.emitter.runningRelease = releaseRunning
	mailbox := NewControlMailbox(8)
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-running"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	f.proc.Exit()
	close(releaseRunning)
	err := <-done
	if !errors.Is(err, context.Canceled) && !hasBackendCode(err, protocol.CodeOperationCancelled) {
		t.Fatalf("Supervise() error = %v, want cancellation", err)
	}
	if f.uv.startCalls != 1 {
		t.Fatalf("StartManaged calls = %d, want 1", f.uv.startCalls)
	}
	if indexOfEvent(f.emitter.eventsSnapshot(), "state:"+string(protocol.StateRestarting)) >= 0 {
		t.Fatal("simultaneous exit and cancel started a restart")
	}
}

func TestBackend_LogCompletenessAcrossRestart(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	first := f.proc
	first.startRecords = []process.StreamRecord{{Stream: process.StreamStdout, Fragment: "first", Event: "first", EndOfLine: true}}
	second := &fakeProcess{pid: 4343, keepAlive: true, startRecords: []process.StreamRecord{{Stream: process.StreamStdout, Fragment: "second", Event: "second", EndOfLine: true}}}
	f.uv.procSequence = []ManagedProcess{first, second}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	first.Exit()
	waitForStateStatusCount(t, f.emitter, protocol.StateRunning, 2)
	if indexOfEvent(f.emitter.eventsSnapshot(), "log:first") < 0 || indexOfEvent(f.emitter.eventsSnapshot(), "log:second") < 0 {
		t.Fatalf("events = %#v, want both restart logs", f.emitter.eventsSnapshot())
	}
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-log"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	<-done
}

func TestBackend_CleansBackendTransactionMutexAndTempState(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	f.depsHTTP = &orderedHTTPCloser{process: f.proc, record: func(string) {}}
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisorWithHTTP(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "shutdown-clean"}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	if f.state.removeCalls == 0 || f.state.closeCalls != 1 || f.lock.closeCalls != 1 || !f.proc.closed || f.logger.closeCalls != 1 {
		t.Fatalf("cleanup state/remove=%d/%d lock=%d processClosed=%v logger=%d", f.state.removeCalls, f.state.closeCalls, f.lock.closeCalls, f.proc.closed, f.logger.closeCalls)
	}
}

func TestBackend_ResourceCloseFailurePrecedesStopped(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.state.closeErr = errors.New("state close failed")
	f.lock.closeErr = errors.New("lock factory close failed")
	f.lock.acquireLease = &fakeLease{closeErr: errors.New("lease close failed")}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-close-failure"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	err := <-done
	assertBackendCode(t, err, protocol.CodeStateWriteFailed)
	states := f.emitter.states()
	if len(states) == 0 || states[len(states)-1].Status != protocol.StateBackendFailed {
		t.Fatalf("states = %#v, want backend_failed as final state", states)
	}
	for _, event := range states {
		if event.Status == protocol.StateStopped {
			t.Fatal("stopped state emitted after resource close failure")
		}
	}
	if f.state.closeCalls != 1 || f.lock.closeCalls != 1 {
		t.Fatalf("resource close calls state/lock = %d/%d, want 1/1", f.state.closeCalls, f.lock.closeCalls)
	}
}

func TestBackend_CancelCleanupFailureDoesNotRedrainControl(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.closeErr = errors.New("process close failed")
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.BeforeControlClose = mailbox.StopAccepting
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-cleanup-failure"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeBackendShutdownFailed)
	case <-timer.C:
		t.Fatal("cancel cleanup failure did not finish within bound")
	}
}

func hasBackendCode(err error, want protocol.Code) bool {
	var coded interface{ Code() protocol.Code }
	return errors.As(err, &coded) && coded.Code() == want
}

func waitForStatusCommand(t *testing.T, emitter *fakeEmitter, commandID string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		for _, event := range emitter.states() {
			if event.Details["controlCommandId"] == commandID {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("status command %q was not emitted", commandID)
		case <-ticker.C:
		}
	}
}

type failingControlReceiver struct {
	err     error
	ready   chan struct{}
	release <-chan struct{}
}

func (r *failingControlReceiver) Receive(ctx context.Context) (protocol.ControlCommand, error) {
	if r.ready != nil {
		select {
		case <-r.ready:
		default:
			close(r.ready)
		}
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return protocol.ControlCommand{}, ctx.Err()
		}
	}
	return protocol.ControlCommand{}, r.err
}

type delayedControlReceiver struct {
	receiver ControlReceiver
	release  <-chan struct{}
}

func (r *delayedControlReceiver) Receive(ctx context.Context) (protocol.ControlCommand, error) {
	select {
	case <-r.release:
		return r.receiver.Receive(ctx)
	case <-ctx.Done():
		return protocol.ControlCommand{}, ctx.Err()
	}
}

func (r *delayedControlReceiver) TerminalCommand() (protocol.ControlCommand, bool) {
	if source, ok := r.receiver.(interface {
		TerminalCommand() (protocol.ControlCommand, bool)
	}); ok {
		return source.TerminalCommand()
	}
	return protocol.ControlCommand{}, false
}

func (r *delayedControlReceiver) InfrastructureError() error {
	if source, ok := r.receiver.(interface{ InfrastructureError() error }); ok {
		return source.InfrastructureError()
	}
	return nil
}

type orderedHTTPCloser struct {
	process *fakeProcess
	record  func(string)
}

type errorHTTPCloser struct{}

func (errorHTTPCloser) Close(context.Context) error { return errors.New("close endpoint unavailable") }

func (c *orderedHTTPCloser) Close(context.Context) error {
	c.record("http")
	return c.process.Terminate(0)
}

func (f *backendFixture) supervisorWithHTTP(ctx context.Context, req Request) error {
	s, err := NewManagedSupervisor(f.layout, Dependencies{
		Lock:            f.lock,
		State:           f.state,
		Repository:      f.repository,
		Entry:           f.entry,
		UV:              f.uv,
		Health:          f.health,
		Logger:          func(context.Context, Request) (Logger, error) { return f.logger, f.loggerErr },
		Clock:           func() time.Time { return time.Unix(1, 0).UTC() },
		UVPath:          "uv.exe",
		PythonPath:      "python.exe",
		PID:             f.pid,
		HTTP:            f.depsHTTP,
		ShutdownTimeout: f.shutdownTimeout,
		NewTimer:        func(time.Duration) Timer { return immediateTimer{} },
	})
	if err != nil {
		return err
	}
	return s.Supervise(ctx, req)
}

func waitForStateStatusCount(t *testing.T, emitter *fakeEmitter, status protocol.StateStatus, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		count := 0
		for _, event := range emitter.states() {
			if event.Status == status {
				count++
			}
		}
		if count >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("state %s count = %d, want %d", status, count, want)
		case <-ticker.C:
		}
	}
}
