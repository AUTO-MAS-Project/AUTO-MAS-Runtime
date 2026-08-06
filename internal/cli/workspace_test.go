package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestWorkspaceSyncCommand_JoinsClosableControlReader(t *testing.T) {
	input := newJoinedControlInput()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(
			context.Background(),
			[]string{"--output", "ndjson", "workspace", "sync", "--version", "v1.0.0"},
			IO{In: input, Out: &stdout, Err: &stderr},
			WithCWD(t.TempDir()),
			WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
				return workspaceTestService{sync: func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
					<-input.readStarted
					return gitrepo.SyncResult{Status: protocol.StateReadyToStart}, nil
				}}, nil
			}),
			WithWorkspaceLoggerFactory(workspaceTestLoggerFactory),
		)
	}()

	<-input.closeCalled
	close(input.allowCloseReturn)
	select {
	case code := <-done:
		t.Fatalf("Execute() returned %d before control reader exited", code)
	case <-time.After(20 * time.Millisecond):
	}
	close(input.releaseRead)
	select {
	case code := <-done:
		if code != protocol.ExitCodeSuccess {
			t.Fatalf("Execute() exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for joined workspace command")
	}
	select {
	case <-input.readDone:
	default:
		t.Fatal("control reader goroutine remains after Execute returned")
	}
}

func TestWorkspaceControlInfrastructureError_PreservesOutputFailure(t *testing.T) {
	err := workspaceControlInfrastructureError(
		protocol.StageWorkspaceSwap,
		fmt.Errorf("control warning: %w", protocol.ErrOutputWriteFailed),
	)
	code, stage, _, _ := classifyFailure(err, protocol.StageWorkspaceClone)
	if code != protocol.CodeOutputWriteFailed {
		t.Fatalf("classifyFailure() code = %s, want %s", code, protocol.CodeOutputWriteFailed)
	}
	if stage != protocol.StageWorkspaceSwap {
		t.Fatalf("classifyFailure() stage = %s, want %s", stage, protocol.StageWorkspaceSwap)
	}
}

func TestWorkspaceSyncResult_MergesLateControlCommandID(t *testing.T) {
	const commandID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	result := gitrepo.SyncResult{Status: protocol.StateEnvironmentBroken}
	result = withWorkspaceControlCommandID(result, commandID)
	details := workspaceSyncDetails(result)
	if got := details["controlCommandId"]; got != commandID {
		t.Fatalf("controlCommandId = %#v, want %q", got, commandID)
	}
}

func TestWorkspaceControlErrorsArePrimaryButBusinessChainPreserved(t *testing.T) {
	controlErr := workspaceControlInfrastructureError(
		protocol.StageWorkspaceCleanup,
		errors.New("control reader failed"),
	)
	businessErr := &commandError{
		code:    protocol.CodeGitCloneFailed,
		stage:   protocol.StageWorkspaceClone,
		message: "克隆失败",
		details: map[string]any{"remote": "test"},
		cause:   errors.New("clone failed"),
	}
	joined := joinWorkspaceControlError(businessErr, controlErr)
	code, stage, _, details := classifyFailure(joined, protocol.StageWorkspaceClone)
	if code != protocol.CodeInternalError {
		t.Fatalf("classifyFailure() code = %s, want %s", code, protocol.CodeInternalError)
	}
	if stage != protocol.StageWorkspaceCleanup {
		t.Fatalf("classifyFailure() stage = %s, want %s", stage, protocol.StageWorkspaceCleanup)
	}
	if details == nil {
		t.Fatal("classifyFailure() details = nil, want non-nil details")
	}
	if !errors.Is(joined, controlErr) {
		t.Fatal("joined error does not preserve control error")
	}
	if !errors.Is(joined, businessErr) {
		t.Fatal("joined error does not preserve business error")
	}
}

func TestWorkspaceControlErrorOutranksBusinessCancellation(t *testing.T) {
	controlErr := workspaceControlInfrastructureError(
		protocol.StageWorkspaceCleanup,
		errors.New("control reader failed"),
	)
	joined := joinWorkspaceControlError(
		&commandError{
			code:    protocol.CodeOperationCancelled,
			stage:   protocol.StageWorkspaceClone,
			message: "操作已取消",
			details: map[string]any{},
			cause:   context.Canceled,
		},
		controlErr,
	)
	code, stage, _, _ := classifyFailure(joined, protocol.StageWorkspaceClone)
	if code != protocol.CodeInternalError || stage != protocol.StageWorkspaceCleanup {
		t.Fatalf("classifyFailure() = %s/%s, want INTERNAL_ERROR/workspace.cleanup", code, stage)
	}
}

func TestWorkspaceControlReaderContextCancellation_DistinguishesReaderOrigin(t *testing.T) {
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	deadlineContext, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelDeadline()
	activeContext := context.Background()
	tests := []struct {
		name       string
		ctx        context.Context
		readErr    error
		wantIgnore bool
	}{
		{
			name:       "reader returned operation sentinel",
			ctx:        canceledContext,
			readErr:    context.Canceled,
			wantIgnore: true,
		},
		{
			name:       "reader returned deadline sentinel",
			ctx:        deadlineContext,
			readErr:    context.DeadlineExceeded,
			wantIgnore: true,
		},
		{
			name:       "reader wrapped sentinel",
			ctx:        canceledContext,
			readErr:    fmt.Errorf("read stdin control: %w", context.Canceled),
			wantIgnore: false,
		},
		{
			name:       "operation sentinel while context active",
			ctx:        activeContext,
			readErr:    context.Canceled,
			wantIgnore: false,
		},
		{
			name:       "reader returned unrelated error",
			ctx:        canceledContext,
			readErr:    errors.New("reader failed"),
			wantIgnore: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isWorkspaceControlContextCancellation(test.ctx, test.readErr); got != test.wantIgnore {
				t.Fatalf("isWorkspaceControlContextCancellation() = %t, want %t", got, test.wantIgnore)
			}
		})
	}
}

func TestStopWorkspaceControl_DoesNotSwallowReaderOriginatedContextError(t *testing.T) {
	var output bytes.Buffer
	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := processOutput.NewEmitter("dev", "workspace sync", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	control := newWorkspaceControl(func() {}, protocol.StageWorkspaceClone)
	reader, err := protocol.NewControlReader(strings.NewReader(""), emitter, control, protocol.ControlCancel)
	if err != nil {
		t.Fatalf("NewControlReader() error = %v", err)
	}
	readerErr := fmt.Errorf("read stdin control: %w", context.Canceled)
	completed := make(chan error, 1)
	completed <- readerErr
	got := stopWorkspaceControl(reader, strings.NewReader(""), completed)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("stopWorkspaceControl() error = %v, want reader cancellation", got)
	}
}

func TestWorkspaceStageEmitter_DoesNotOverwriteBusinessStage(t *testing.T) {
	var output bytes.Buffer
	processOutput, err := protocol.NewProcessOutput(&output)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := processOutput.NewEmitter("dev", "workspace sync", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	control := newWorkspaceControl(func() {}, protocol.StageWorkspaceVerify)
	stageEmitter := &workspaceStageEmitter{emitter: emitter, control: control}
	if err := stageEmitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageWorkspaceClone,
		Status:  protocol.ProgressRunning,
		Message: "正在接收后端仓库数据",
	}); err != nil {
		t.Fatalf("EmitProgress() error = %v", err)
	}
	if got := control.CurrentControlStage(); got != protocol.StageWorkspaceVerify {
		t.Fatalf("control stage = %q, want %q", got, protocol.StageWorkspaceVerify)
	}
}

type joinedControlInput struct {
	readStarted      chan struct{}
	releaseRead      chan struct{}
	readDone         chan struct{}
	closeCalled      chan struct{}
	allowCloseReturn chan struct{}
}

func newJoinedControlInput() *joinedControlInput {
	return &joinedControlInput{
		readStarted:      make(chan struct{}),
		releaseRead:      make(chan struct{}),
		readDone:         make(chan struct{}),
		closeCalled:      make(chan struct{}),
		allowCloseReturn: make(chan struct{}),
	}
}

func (i *joinedControlInput) Read([]byte) (int, error) {
	close(i.readStarted)
	<-i.releaseRead
	close(i.readDone)
	return 0, io.EOF
}

func (i *joinedControlInput) Close() error {
	close(i.closeCalled)
	<-i.allowCloseReturn
	return nil
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
