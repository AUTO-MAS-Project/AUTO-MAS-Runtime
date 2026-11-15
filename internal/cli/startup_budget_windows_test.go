//go:build windows

package cli

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

func TestBackendSupervise_InternalStartupDeadlineKeepsFailureTuple(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	var stdout, stderr bytes.Buffer
	var ticks atomic.Int64
	origin := time.Now()
	code := Execute(t.Context(), []string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: input, Out: &stdout, Err: &stderr},
		WithBackendFactory(func(ctx context.Context, layout *config.Layout, diagnostics io.Writer, _ func() time.Time, policy mirror.Policy) (backendService, error) {
			// 只推进监督器的工作时钟，CLI 调用方上下文保持有效。
			return backend.NewProductionManagedSupervisor(ctx, layout, diagnostics, func() time.Time {
				return origin.Add(time.Duration(ticks.Add(1)) * time.Hour)
			}, policy)
		}))
	if code != 60 {
		t.Fatalf("exit = %d, want 60; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	contracttest.Assert(t, "backend supervise", contracttest.TerminalFailure, stdout.Bytes())
	for _, event := range parseNDJSON(t, stdout.String()) {
		if eventType(event) != "error" && eventType(event) != "result" {
			continue
		}
		if eventString(event, "code") != string(protocol.CodeBackendHealthTimeout) || eventString(event, "stage") != string(protocol.StageBackendSpawn) {
			t.Fatalf("unexpected failure tuple: %#v", event.object)
		}
		if eventType(event) == "result" && eventString(event, "status") != "backend_failed" {
			t.Fatalf("unexpected terminal result: %#v", event.object)
		}
	}
}
