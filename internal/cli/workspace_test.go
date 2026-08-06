package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestWorkspaceSyncCommand_ValidatesVersion(t *testing.T) {
	t.Parallel()
	var factoryCalls int
	options := []Option{
		WithCWD(t.TempDir()),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			factoryCalls++
			return workspaceTestService{}, nil
		}),
	}
	for _, args := range [][]string{
		{"--output", "ndjson", "workspace", "sync"},
		{"--output", "ndjson", "workspace", "sync", "--version", "v1.0.0", "--version", "v1.0.1"},
	} {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), args, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, options...)
		if code != protocol.ExitCodeInvalidArgument {
			t.Errorf("args=%v exit code = %d, want %d", args, code, protocol.ExitCodeInvalidArgument)
		}
		events := parseNDJSON(t, stdout.String())
		if got := eventString(events[len(events)-1], "code"); got != string(protocol.CodeInvalidVersion) {
			t.Errorf("args=%v result code = %q, want INVALID_VERSION", args, got)
		}
	}
	if factoryCalls != 0 {
		t.Fatalf("workspace factory calls = %d, want 0 for invalid versions", factoryCalls)
	}
}

func TestWorkspaceSyncCommand_CancelFromStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	commandID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	input := `{"protocol":1,"command":"cancel","commandId":"` + commandID + `"}` + "\n"
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "workspace", "sync", "--version", "v1.0.0"},
		IO{In: strings.NewReader(input), Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			return workspaceTestService{sync: func(ctx context.Context, _ gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				<-ctx.Done()
				return gitrepo.SyncResult{}, ctx.Err()
			}}, nil
		}),
		WithWorkspaceLoggerFactory(workspaceTestLoggerFactory),
	)
	if code != protocol.ExitCodeOperationCancelled {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationCancelled)
	}
	events := parseNDJSON(t, stdout.String())
	result := events[len(events)-1]
	if got := eventString(result, "code"); got != string(protocol.CodeOperationCancelled) {
		t.Fatalf("result code = %q, want OPERATION_CANCELLED", got)
	}
	details, ok := result.object["details"].(map[string]any)
	if !ok || details["controlCommandId"] != commandID {
		t.Fatalf("result details = %#v, want commandId", result.object["details"])
	}
}

func TestWorkspaceSyncCommand_ResultStatusMatchesLifecycle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "workspace", "sync", "--version", "v1.0.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			return workspaceTestService{sync: func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				return gitrepo.SyncResult{
					Changed: true,
					Status:  protocol.StateEnvironmentBroken,
				}, nil
			}}, nil
		}),
		WithWorkspaceLoggerFactory(workspaceTestLoggerFactory),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit code = %d, want 0", code)
	}
	events := parseNDJSON(t, stdout.String())
	result := events[len(events)-1]
	if got := eventString(result, "status"); got != string(protocol.StateEnvironmentBroken) {
		t.Fatalf("result status = %q, want environment_broken", got)
	}
	if changed, ok := result.object["details"].(map[string]any)["changed"].(bool); !ok || !changed {
		t.Fatalf("result changed = %#v, want true", result.object["details"])
	}
}

type workspaceTestService struct {
	check func(context.Context) (gitrepo.CheckResult, error)
	sync  func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error)
}

func (s workspaceTestService) Check(ctx context.Context) (gitrepo.CheckResult, error) {
	if s.check != nil {
		return s.check(ctx)
	}
	return gitrepo.CheckResult{Healthy: true, Reason: "ok"}, nil
}

func (s workspaceTestService) Sync(ctx context.Context, request gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
	if s.sync != nil {
		return s.sync(ctx, request)
	}
	return gitrepo.SyncResult{}, errors.New("workspace test service sync not configured")
}

type workspaceTestLogger struct{}

func (workspaceTestLogger) LogPath() string { return "C:\\runtime\\workspace-sync.log" }
func (workspaceTestLogger) Close() error    { return nil }
func (workspaceTestLogger) Record(context.Context, logging.Level, string, map[string]any) (logging.WriteResult, error) {
	return logging.WriteResult{}, nil
}

func workspaceTestLoggerFactory(
	_ context.Context,
	_ *config.Layout,
	_ io.Writer,
	_ string,
	_ string,
	_ func() time.Time,
) (workspaceLogger, error) {
	return workspaceTestLogger{}, nil
}
