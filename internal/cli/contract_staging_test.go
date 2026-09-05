package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

type stagingContractService struct {
	workspaceTestService
	err  error
	wait bool
}

func (s stagingContractService) Stage(ctx context.Context, _ gitrepo.StageRequest) (gitrepo.StageResult, error) {
	if s.wait {
		<-ctx.Done()
		return gitrepo.StageResult{}, ctx.Err()
	}
	revision, err := gitrepo.NewRevision("v1.0.0", "release/v1.0.0", strings.Repeat("b", 40), "github")
	if err != nil {
		return gitrepo.StageResult{}, err
	}
	return gitrepo.StageResult{Revision: revision, Staged: s.err == nil}, s.err
}

func (s stagingContractService) CheckRemote(context.Context, mirror.Policy) (gitrepo.RemoteCheckResult, error) {
	return gitrepo.RemoteCheckResult{Current: gitrepo.CheckResult{Healthy: true, Version: "v1.0.0", Commit: strings.Repeat("a", 40)}, RemoteCommit: strings.Repeat("b", 40), UpdateAvailable: true}, s.err
}

func TestWorkspaceStageContract(t *testing.T) {
	contracttest.Register(t, "workspace stage", stagingContractRunner(false))
}

func TestWorkspaceRemoteContract(t *testing.T) {
	contracttest.Register(t, "workspace check --remote", stagingContractRunner(true))
}

func stagingContractRunner(remote bool) contracttest.Runner {
	return func(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
		t.Helper()
		ctx := t.Context()
		service := stagingContractService{}
		wantExit := protocol.ExitCodeSuccess
		if terminal == contracttest.TerminalFailure {
			service.err = &commandError{code: protocol.CodeGitRemoteResolveFailed, stage: protocol.StageWorkspaceCheck, message: "无法查询远端分支", details: map[string]any{}}
			definition, _ := protocol.LookupErrorDefinition(protocol.CodeGitRemoteResolveFailed)
			wantExit = definition.ExitCode
		}
		if terminal == contracttest.TerminalCancelled {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
			wantExit = protocol.ExitCodeOperationCancelled
		}
		args := []string{"--output", "ndjson", "workspace", "stage", "--version", "v1.0.0"}
		if remote {
			args = []string{"--output", "ndjson", "workspace", "check", "--remote"}
		}
		var stdout, stderr bytes.Buffer
		code := Execute(ctx, args, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(t.TempDir()),
			WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return service, nil }), WithWorkspaceLoggerFactory(workspaceTestLoggerFactory))
		if code != wantExit {
			t.Fatalf("exit=%d, want %d: %s", code, wantExit, stdout.String())
		}
		return contracttest.Transcript{Stdout: stdout.Bytes()}
	}
}

func TestWorkspaceStage_StdinCancel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	input := "{\"protocol\":1,\"command\":\"cancel\",\"commandId\":\"01ARZ3NDEKTSV4RRFFQ69G5FAV\"}\n"
	code := Execute(t.Context(), []string{"--output", "ndjson", "workspace", "stage", "--version", "v1.0.0"},
		IO{In: strings.NewReader(input), Out: &stdout, Err: &stderr}, WithCWD(t.TempDir()),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return stagingContractService{wait: true}, nil }))
	if code != protocol.ExitCodeOperationCancelled {
		t.Fatalf("exit=%d, want cancellation: %s", code, stdout.String())
	}
}
