//go:build linux || darwin

package uv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestPOSIXRunner_DrainsBeforeClose(t *testing.T) {
	runner := newTestRunner(t)
	payload := strings.Repeat("record\n", 900)
	first := true
	result, err := runner.Run(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
		Stage:       protocol.StageUVCheck,
		Environment: map[string]string{"FAKE_UV_STDOUT": payload},
		Line: func(ctx context.Context, _, _ string) error {
			if first {
				first = false
				timer := time.NewTimer(200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	})
	if err != nil || result.Stdout != payload {
		t.Fatalf("Run got %d/%d bytes and %v, want complete successful output", len(result.Stdout), len(payload), err)
	}
}

func TestPOSIXRunner_CleanupCancelsCallback(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.Run(ctx, []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{
			Stage:       protocol.StageUVCheck,
			Environment: map[string]string{"FAKE_UV_STDOUT": "one\n"},
			Line: func(callbackCtx context.Context, _, _ string) error {
				close(entered)
				<-callbackCtx.Done()
				return callbackCtx.Err()
			},
		})
	}()
	<-entered
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		cancel()
		<-done
		t.Fatal("Run remained blocked after its five-second cleanup budget with a callback honoring cancellation")
	}
}
