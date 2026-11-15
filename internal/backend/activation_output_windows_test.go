//go:build windows

package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type activationOutputFailure struct{ *fakeEmitter }

func (e *activationOutputFailure) EmitState(event protocol.StateEvent) error {
	if event.Details["startupActivation"] == "active" || event.Details["startupActivation"] == "unknown" {
		return errors.New("broken pipe")
	}
	return e.fakeEmitter.EmitState(event)
}

func TestDeferredStartup_ActivationOutputFailureReleasesProcess(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	declareDeferredStartup(t, f.layout.RepoDir())
	mailbox := NewControlMailbox(8)
	supervisor := f.supervisor()
	supervisor.deps.Activation = &recordingActivator{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	request := f.request()
	request.Control = mailbox
	request.DeferBusinessStart = true
	request.Emitter = &activationOutputFailure{f.emitter}
	go func() { done <- supervisor.Supervise(ctx, request) }()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(ctx, protocol.ControlCommand{Command: protocol.ControlActivate, CommandID: "activate"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeOutputWriteFailed)
		t.Logf("error=%v terminated=%v waitedEmpty=%v closed=%v", err, f.proc.terminated, f.proc.waitedEmpty, f.proc.closed)
		if !f.proc.closed || !f.proc.waitedEmpty {
			t.Fatal("supervision returned before process cleanup")
		}
		if f.state.removeCalls != 1 || f.logger.closeCalls != 1 {
			t.Fatalf("transaction/logger cleanup = %d/%d, want 1/1", f.state.removeCalls, f.logger.closeCalls)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("supervision did not settle")
	}
}
