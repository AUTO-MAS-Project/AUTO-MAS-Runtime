package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
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
	return gitrepo.StageResult{Revision: revision, Staged: s.err == nil, CommitMessage: "修复后台更新提示"}, s.err
}

func TestWorkspaceStage_CommitMessageInResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "ndjson", "workspace", "stage", "--version", "v1.0.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(t.TempDir()),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return stagingContractService{}, nil }), WithWorkspaceLoggerFactory(workspaceTestLoggerFactory))
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("exit = %d, want success: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"commitMessage":"修复后台更新提示"`) {
		t.Fatalf("stage result has no commit message: %s", stdout.String())
	}
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

func TestWorkspaceRemote_FailureDiagnostics(t *testing.T) {
	attempts := []map[string]any{
		{"source": "cnb", "sourceTry": 1, "globalTry": 1, "outcome": "switch_source", "failureKind": "dns_failed", "durationMs": int64(125)},
		{"source": "github", "sourceTry": 1, "globalTry": 2, "outcome": "switch_source", "failureKind": "http_status", "httpStatus": 503, "durationMs": int64(250)},
	}
	service := stagingContractService{err: &commandError{
		code: protocol.CodeGitRemoteResolveFailed, stage: protocol.StageWorkspaceCheck,
		message: "无法查询远端分支", details: map[string]any{"attempts": attempts},
	}}
	recorder := &telemetryRecorderSpy{}
	var stdout bytes.Buffer
	code := executeWithTelemetry(t, []string{"--output", "ndjson", "workspace", "check", "--remote"}, recorder, &stdout,
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return service, nil }),
		WithWorkspaceLoggerFactory(workspaceTestLoggerFactory))
	definition, _ := protocol.LookupErrorDefinition(protocol.CodeGitRemoteResolveFailed)
	if code != definition.ExitCode {
		t.Fatalf("exit = %d, want %d: %s", code, definition.ExitCode, stdout.String())
	}
	seen := map[string]int{}
	var errorDetails map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
		var event struct {
			Type        string                 `json:"type"`
			Code        string                 `json:"code"`
			Stage       protocol.Stage         `json:"stage"`
			Retryable   bool                   `json:"retryable"`
			Remediation []protocol.Remediation `json:"remediation"`
			Details     map[string]any         `json:"details"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "error" && event.Type != "result" {
			continue
		}
		seen[event.Type]++
		if event.Code != string(protocol.CodeGitRemoteResolveFailed) || event.Stage != protocol.StageWorkspaceCheck ||
			event.Retryable != definition.Retryable || !reflect.DeepEqual(event.Remediation, definition.Remediation) {
			t.Errorf("event = %s, want original error contract at workspace.check", line)
		}
		if event.Type == "error" {
			errorDetails = event.Details
		}
		if !reflect.DeepEqual(event.Details, errorDetails) {
			t.Errorf("details = %#v, want error details %#v", event.Details, errorDetails)
		}
	}
	if seen["error"] != 1 || seen["result"] != 1 {
		t.Fatalf("terminal events = %#v, want one error and one result", seen)
	}
	observations, _, _ := recorder.snapshot()
	wantDiagnostics := telemetry.SanitizeDiagnostics(map[string]any{"attempts": attempts})
	if len(observations) != 1 || observations[0].Command != "workspace check" ||
		observations[0].Stage != string(protocol.StageWorkspaceCheck) ||
		!reflect.DeepEqual(observations[0].DiagnosticDetails, wantDiagnostics) {
		t.Fatalf("observations = %#v, want check stage and complete attempt diagnostics", observations)
	}
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
