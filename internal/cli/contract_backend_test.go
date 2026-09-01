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
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

func TestBackendSuperviseContract(t *testing.T) {
	contracttest.Register(t, "backend supervise", backendContractRunner())
}

// TestBackendSupervise_AdvertisesEveryAcceptedControlCommand 钉住「公告集合 ==
// 实际接受的控制命令集合」。此前 backend supervise 只公告 cancel/state/log，
// 却真实接受 shutdown 与 status，而 README 与架构文档都写「实际可用命令以
// hello.capabilities 为准」——照文档写的客户端永远不会发 shutdown，优雅关闭
// 会退化成 Job 强杀。这条测试让公告与 ControlReader 的注册列表不能再分叉。
func TestBackendSupervise_AdvertisesEveryAcceptedControlCommand(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(context.Context, backend.Request) error { return nil }), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	events := parseNDJSON(t, stdout.String())
	if len(events) == 0 || eventType(events[0]) != string(protocol.TypeHello) {
		t.Fatalf("first event = %#v, want hello", events)
	}
	raw, ok := events[0].object["capabilities"].([]any)
	if !ok {
		t.Fatalf("hello capabilities = %#v, want array", events[0].object["capabilities"])
	}
	announced := make(map[string]bool, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("capability %#v is not a string", value)
		}
		if !protocol.IsKnownCapability(protocol.Capability(text)) {
			t.Errorf("capability %q is not part of the frozen set", text)
		}
		announced[text] = true
	}
	// 与 runBackendSuperviseSession 里 NewControlReader 注册的命令一一对应。
	for command, capability := range map[protocol.ControlKind]protocol.Capability{
		protocol.ControlCancel:   protocol.CapabilityStdinCancel,
		protocol.ControlShutdown: protocol.CapabilityStdinShutdown,
		protocol.ControlStatus:   protocol.CapabilityStdinStatus,
	} {
		if !announced[string(capability)] {
			t.Errorf("hello capabilities = %#v, want %q for accepted control command %q", raw, capability, command)
		}
	}
	for _, capability := range []protocol.Capability{protocol.CapabilityStateV1, protocol.CapabilityLogStream} {
		if !announced[string(capability)] {
			t.Errorf("hello capabilities = %#v, want %q", raw, capability)
		}
	}
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
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
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
