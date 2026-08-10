package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

func TestBackendSuperviseContract(t *testing.T) {
	contracttest.Register(t, "backend supervise", backendContractRunner())
}

func backendContractRunner() contracttest.Runner {
	return func(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
		t.Helper()
		ctx := context.Background()
		wantExit := protocol.ExitCodeSuccess
		service := backendServiceFunc(func(_ context.Context, request backend.Request) error {
			if err := request.Emitter.EmitState(protocol.StateEvent{
				Stage: protocol.StageBackendSpawn, Status: protocol.StateStartingBackend,
				Message: "正在启动后端", Details: map[string]any{},
			}); err != nil {
				return err
			}
			return request.Emitter.EmitState(protocol.StateEvent{
				Stage: protocol.StageBackendRun, Status: protocol.StateRunning,
				Message: "后端已就绪", Details: map[string]any{
					"pid": uint32(42), "baseUrl": "http://127.0.0.1:36163", "logPath": "backend.log",
				},
			})
		})
		switch terminal {
		case contracttest.TerminalSuccess:
		case contracttest.TerminalFailure:
			service = backendServiceFunc(func(_ context.Context, request backend.Request) error {
				if err := request.Emitter.EmitLog(protocol.LogEvent{
					Source: "backend", Stream: "stderr", Message: "tail",
				}); err != nil {
					return err
				}
				return backendContractError{}
			})
			definition, ok := protocol.LookupErrorDefinition(protocol.CodeBackendSpawnFailed)
			if !ok {
				t.Fatal("BACKEND_SPAWN_FAILED definition is missing")
			}
			wantExit = definition.ExitCode
		case contracttest.TerminalCancelled:
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			ctx = cancelled
			wantExit = protocol.ExitCodeOperationCancelled
		default:
			t.Fatalf("unexpected terminal %q", terminal)
		}

		root := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := Execute(
			ctx,
			[]string{"--app-root", root, "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
			IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
			WithCWD(root),
			WithClock(func() time.Time { return time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC) }),
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
				return service, nil
			}),
		)
		if code != wantExit {
			t.Errorf("exit code = %d, want %d; stderr=%q", code, wantExit, stderr.String())
		}
		if terminal == contracttest.TerminalFailure {
			events := parseNDJSON(t, stdout.String())
			if got := eventString(events[len(events)-1], "status"); got != string(protocol.StateBackendFailed) {
				t.Errorf("failure result status = %q, want backend_failed", got)
			}
		}
		return contracttest.Transcript{Stdout: stdout.Bytes()}
	}
}

type backendContractError struct{}

func (backendContractError) Error() string { return "injected backend spawn failure" }

func (backendContractError) Code() protocol.Code { return protocol.CodeBackendSpawnFailed }

func (backendContractError) Stage() protocol.Stage { return protocol.StageBackendSpawn }

func (backendContractError) Message() string { return "后端进程启动失败" }

func (backendContractError) Details() map[string]any { return map[string]any{} }

func (backendContractError) TerminalStatus() string { return string(protocol.StateBackendFailed) }
