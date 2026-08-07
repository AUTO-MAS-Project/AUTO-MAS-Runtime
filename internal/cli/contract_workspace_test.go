package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

// TestWorkspaceContract 以通用契约测试固定 workspace sync 的三种终态。
func TestWorkspaceContract(t *testing.T) {
	contracttest.Register(t, "workspace sync", workspaceContractRunner)
}

// TestWorkspaceCheckContract 以通用契约测试固定 workspace check 的三种终态。
func TestWorkspaceCheckContract(t *testing.T) {
	contracttest.Register(t, "workspace check", workspaceCheckContractRunner)
}

func workspaceCheckContractRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
	t.Helper()
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	service := workspaceTestService{}
	wantExit := protocol.ExitCodeSuccess
	switch terminal {
	case contracttest.TerminalSuccess:
		service.check = func(context.Context) (gitrepo.CheckResult, error) {
			return gitrepo.CheckResult{
				Healthy: true,
				Version: "v1.0.0",
				Branch:  "release/v1.0.0",
				Commit:  strings.Repeat("a", 40),
				Source:  "github",
				Reason:  "ok",
			}, nil
		}
	case contracttest.TerminalFailure:
		service.check = func(context.Context) (gitrepo.CheckResult, error) {
			return gitrepo.CheckResult{}, &commandError{
				code:    protocol.CodeGitRepositoryInvalid,
				stage:   protocol.StageWorkspaceCheck,
				message: "受管后端仓库无效",
				details: map[string]any{},
			}
		}
		definition, _ := protocol.LookupErrorDefinition(protocol.CodeGitRepositoryInvalid)
		wantExit = definition.ExitCode
	case contracttest.TerminalCancelled:
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ctx = cancelled
		wantExit = protocol.ExitCodeOperationCancelled
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}
	code := Execute(
		ctx,
		[]string{"--output", "ndjson", "workspace", "check"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()),
		WithClock(func() time.Time { return time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC) }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return service, nil }),
		WithWorkspaceLoggerFactory(workspaceTestLoggerFactory),
	)
	if code != wantExit {
		t.Errorf("exit code = %d, want %d", code, wantExit)
	}
	return contracttest.Transcript{Stdout: stdout.Bytes()}
}

func workspaceContractRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
	t.Helper()
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	service := workspaceTestService{}
	switch terminal {
	case contracttest.TerminalSuccess:
		service.sync = func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
			return gitrepo.SyncResult{Status: protocol.StateEnvironmentBroken, Changed: true}, nil
		}
	case contracttest.TerminalFailure:
		service.sync = func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
			return gitrepo.SyncResult{}, &commandError{
				code:    protocol.CodeGitCloneFailed,
				stage:   protocol.StageWorkspaceClone,
				message: "无法克隆后端仓库",
				details: map[string]any{},
			}
		}
	case contracttest.TerminalCancelled:
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ctx = cancelled
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}
	code := Execute(
		ctx,
		[]string{"--output", "ndjson", "workspace", "sync", "--version", "v1.0.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(t.TempDir()),
		WithClock(func() time.Time { return time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC) }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return service, nil }),
		WithWorkspaceLoggerFactory(workspaceTestLoggerFactory),
	)
	wantExit := protocol.ExitCodeSuccess
	if terminal == contracttest.TerminalFailure {
		definition, _ := protocol.LookupErrorDefinition(protocol.CodeGitCloneFailed)
		wantExit = definition.ExitCode
	}
	if terminal == contracttest.TerminalCancelled {
		wantExit = protocol.ExitCodeOperationCancelled
	}
	if code != wantExit {
		t.Errorf("exit code = %d, want %d", code, wantExit)
	}
	return contracttest.Transcript{Stdout: stdout.Bytes()}
}
