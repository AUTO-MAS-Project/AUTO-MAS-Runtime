package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestM5CommandCapabilitiesMatchStateOwnership(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantState bool
	}{
		{name: "bootstrap", args: []string{"bootstrap", "--version", "v5.4.0"}, wantState: true},
		{name: "environment check", args: []string{"environment", "check"}},
		{name: "environment ensure", args: []string{"environment", "ensure"}, wantState: true},
		{name: "environment repair", args: []string{"environment", "repair"}, wantState: true},
		{name: "dependencies check", args: []string{"dependencies", "check"}},
		{name: "dependencies sync", args: []string{"dependencies", "sync"}, wantState: true},
		{name: "dependencies rebuild", args: []string{"dependencies", "rebuild"}, wantState: true},
		{name: "repair", args: []string{"repair"}, wantState: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			args := append([]string{"--output", "ndjson"}, test.args...)
			result := runCLI(t, ctx, args...)
			events := parseNDJSON(t, result.stdout)
			if len(events) == 0 || eventType(events[0]) != string(protocol.TypeHello) {
				t.Fatalf("first event = %#v, want hello", events)
			}
			capabilities, ok := events[0].object["capabilities"].([]any)
			if !ok {
				t.Fatalf("hello capabilities = %#v, want array", events[0].object["capabilities"])
			}
			if !containsCapability(capabilities, protocol.CapabilityStdinCancel) {
				t.Fatal("hello capabilities do not advertise stdin.cancel")
			}
			if got := containsCapability(capabilities, protocol.CapabilityStateV1); got != test.wantState {
				t.Fatalf("state.v1 capability = %t, want %t; capabilities=%#v", got, test.wantState, capabilities)
			}
		})
	}
}

func TestM5HelpDefinesRepairScope(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "top-level repair covers every managed tool layer",
			args: []string{"repair", "--help"},
			want: []string{"uv", "Python", "venv", "锁定依赖"},
		},
		{
			name: "dependency rebuild excludes uv and Python repair",
			args: []string{"dependencies", "rebuild", "--help"},
			want: []string{"仅", "venv", "锁定依赖", "不修复 uv/Python"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runCLI(t, context.Background(), test.args...)
			if result.exitCode != protocol.ExitCodeSuccess {
				t.Fatalf("help exit code = %d, want success; stderr=%q", result.exitCode, result.stderr)
			}
			for _, want := range test.want {
				if !strings.Contains(result.stdout, want) {
					t.Errorf("help output does not contain %q: %s", want, result.stdout)
				}
			}
		})
	}
}

func containsCapability(values []any, want protocol.Capability) bool {
	for _, value := range values {
		if value == string(want) {
			return true
		}
	}
	return false
}

func TestBootstrapCommand_OrderAndStates(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{
		calls: &log.calls,
		uvProgress: []mirror.DownloadProgress{
			{Received: 512, Total: 1024, Percent: 50},
		},
	}
	workspace := &m5TestWorkspace{calls: &log.calls, emitStates: true}
	store := &m5TestStateStore{calls: &log.calls}
	coordinator := &m5TestCoordinator{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{
			"--app-root", root,
			"--output", "ndjson",
			"--mirror", "python=github",
			"--mirror", "git=cnb",
			"bootstrap", "--version", "v5.4.0",
		},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return coordinator, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", code, protocol.ExitCodeSuccess, stderr.String())
	}
	if got, want := strings.Join(log.calls, ","), "acquire,uv,workspace,python-spec,python,dependencies,ready,logger-close,lease-close,coordinator-close,store-close"; got != want {
		t.Fatalf("call order = %q, want %q", got, want)
	}
	events := parseNDJSON(t, stdout.String())
	var statuses []string
	for _, event := range events {
		if eventType(event) == string(protocol.TypeState) {
			statuses = append(statuses, eventString(event, "status"))
		}
	}
	wantStatuses := []string{
		string(protocol.StatePreparingUV),
		string(protocol.StateSyncingRepository),
		string(protocol.StatePreparingPython),
		string(protocol.StateSyncingEnvironment),
		string(protocol.StateReadyToStart),
	}
	if strings.Join(statuses, ",") != strings.Join(wantStatuses, ",") {
		t.Fatalf("state statuses = %#v, want %#v", statuses, wantStatuses)
	}
	assertMeasuredProgress(t, events, protocol.StageUVDownload, 512, 1024, 50)
	assertProgressLifecycle(t, events, protocol.StagePythonInstall)
	assertProgressLifecycle(t, events, protocol.StageDependenciesSync)
	if len(store.writes) != 1 || store.writes[0].Status != protocol.StateReadyToStart {
		t.Fatalf("state writes = %#v, want one ready_to_start write", store.writes)
	}
	if got, ok := environment.uvPolicy.Preferred(mirror.KindPython); !ok || got != "github" {
		t.Fatalf("uv policy Python preference = %q/%t, want github/true", got, ok)
	}
	if got, ok := environment.pythonRequest.MirrorPolicy.Preferred(mirror.KindPython); !ok || got != "github" {
		t.Fatalf("Python request preference = %q/%t, want github/true", got, ok)
	}
	// 用 git 首选验证依赖请求的策略透传路径；package-index 首选的透传与「不再拒绝」
	// 由 TestM5CommandsAcceptPackageIndexPreference 覆盖。
	if got, ok := environment.dependencyRequest.MirrorPolicy.Preferred(mirror.KindGit); !ok || got != "cnb" {
		t.Fatalf("dependency request preference = %q/%t, want cnb/true", got, ok)
	}
}

func assertMeasuredProgress(
	t *testing.T,
	events []parsedEvent,
	stage protocol.Stage,
	wantCurrent int64,
	wantTotal int64,
	wantPercent float64,
) {
	t.Helper()
	for _, event := range events {
		if eventType(event) != string(protocol.TypeProgress) ||
			eventString(event, "stage") != string(stage) {
			continue
		}
		current, currentOK := event.object["current"].(float64)
		total, totalOK := event.object["total"].(float64)
		percent, percentOK := event.object["percent"].(float64)
		if currentOK && totalOK && percentOK &&
			int64(current) == wantCurrent && int64(total) == wantTotal && percent == wantPercent {
			return
		}
	}
	t.Fatalf(
		"%s measured progress not found; want current=%d total=%d percent=%v",
		stage,
		wantCurrent,
		wantTotal,
		wantPercent,
	)
}

func assertProgressLifecycle(t *testing.T, events []parsedEvent, stage protocol.Stage) {
	t.Helper()
	statuses := make([]string, 0, 2)
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) &&
			eventString(event, "stage") == string(stage) {
			statuses = append(statuses, eventString(event, "status"))
		}
	}
	want := []string{string(protocol.ProgressRunning), string(protocol.ProgressSucceeded)}
	if len(statuses) < 2 || statuses[0] != want[0] || statuses[len(statuses)-1] != want[1] {
		t.Fatalf("%s progress statuses = %#v, want %#v", stage, statuses, want)
	}
}

func TestUVDownloadProgress_OutputFailureMapsToOutputWriteFailed(t *testing.T) {
	writer := &measuredProgressFailingWriter{}
	output, err := protocol.NewProcessOutput(writer)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("dev", "bootstrap", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}

	err = uvDownloadProgress(emitter)(mirror.DownloadProgress{
		Received: 512,
		Total:    1024,
		Percent:  50,
	})
	var operationErr *commandError
	if !errors.As(err, &operationErr) {
		t.Fatalf("progress error = %T %v, want commandError", err, err)
	}
	if operationErr.Code() != protocol.CodeOutputWriteFailed {
		t.Fatalf("progress code = %s, want %s", operationErr.Code(), protocol.CodeOutputWriteFailed)
	}
}

type measuredProgressFailingWriter struct {
	bytes.Buffer
}

func (w *measuredProgressFailingWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte(`"current":512`)) {
		return 0, errors.New("injected measured progress write failure")
	}
	return w.Buffer.Write(value)
}

// TestM5CommandsAcceptPackageIndexPreference 锁定增补 1 C10 的 2026-09-01 修订：
// bootstrap 与顶层 repair 不再对显式 --mirror package-index=<键> 返回 INVALID_ARGUMENT，
// 而是把它原样透传给依赖服务，由后者排在镜像尝试顺序最前。
// 修订理由：改写锁副本不是覆盖索引，--locked 校验仍对原锁做，因此显式指定与自动轮换
// 本就是同一条路径，只差顺序；保留拒绝会让用户想优先用某个可达镜像时反被判参数错误。
func TestM5CommandsAcceptPackageIndexPreference(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bootstrap", args: []string{"bootstrap", "--version", "v5.4.0"}},
		{name: "repair", args: []string{"repair"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log := &m5TestLog{}
			environment := &m5TestEnvironment{calls: &log.calls}
			workspace := &m5TestWorkspace{calls: &log.calls}
			store := &m5TestStateStore{calls: &log.calls}
			coordinator := &m5TestCoordinator{calls: &log.calls}
			var stdout, stderr bytes.Buffer
			args := append(
				[]string{
					"--app-root", root,
					"--output", "ndjson",
					"--mirror", "package-index=aliyun",
				},
				test.args...,
			)
			code := Execute(
				context.Background(),
				args,
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithCWD(root),
				WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
				WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
				WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
					return store, nil
				}),
				WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
					return coordinator, nil
				}),
				WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
					return log, nil
				}),
			)
			if code != int(protocol.ExitCodeSuccess) {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, protocol.ExitCodeSuccess, stderr.String())
			}
			if environment.syncCalls != 1 {
				t.Fatalf("dependency sync calls = %d, want 1", environment.syncCalls)
			}
			got, ok := environment.dependencyRequest.MirrorPolicy.Preferred(mirror.KindPackageIndex)
			if !ok || got != "aliyun" {
				t.Fatalf("dependency request package-index preference = %q/%t, want aliyun/true", got, ok)
			}
			events := parseNDJSON(t, stdout.String())
			for _, event := range events {
				if eventType(event) == string(protocol.TypeError) {
					t.Fatalf("unexpected error event: %#v", event.object)
				}
			}
			if got := eventString(events[len(events)-1], "code"); got != "OK" {
				t.Errorf("result code = %q, want OK", got)
			}
		})
	}
}

func TestBootstrapCommand_FailurePersistsBroken(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls, dependencyErr: errors.New("fake sync failure")}
	workspace := &m5TestWorkspace{calls: &log.calls}
	store := &m5TestStateStore{calls: &log.calls}
	coordinator := &m5TestCoordinator{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return coordinator, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code == protocol.ExitCodeSuccess {
		t.Fatal("Execute() exit code = success, want dependency failure")
	}
	if environment.syncCalls != 1 || environment.readyCalls != 0 {
		t.Fatalf("environment calls = sync %d ready %d, want short-circuit after one sync", environment.syncCalls, environment.readyCalls)
	}
	if len(store.writes) != 1 || store.writes[0].Status != protocol.StateEnvironmentBroken {
		t.Fatalf("state writes = %#v, want one environment_broken write", store.writes)
	}
	if store.writes[0].Broken == nil || store.writes[0].Broken.Reason != state.ReasonOperationFailed {
		t.Fatalf("broken state = %#v, want operation_failed", store.writes[0].Broken)
	}
	if strings.Contains(strings.Join(log.calls, ","), "ready") {
		t.Fatalf("calls = %#v, must not reach ready", log.calls)
	}
}

func TestEnvironmentEnsure_CancelFromStdin(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls, waitForCancel: true}
	store := &m5TestStateStore{calls: &log.calls}
	coordinator := &m5TestCoordinator{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	commandID := "01J00000000000000000000000"
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "ensure"},
		IO{
			In:  strings.NewReader(`{"protocol":1,"command":"cancel","commandId":"` + commandID + `"}` + "\n"),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return coordinator, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeOperationCancelled {
		t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", code, protocol.ExitCodeOperationCancelled, stderr.String())
	}
	events := parseNDJSON(t, stdout.String())
	var sawCapability, sawCommandID bool
	for _, event := range events {
		if eventType(event) == string(protocol.TypeHello) {
			capabilities, _ := event.object["capabilities"].([]any)
			for _, value := range capabilities {
				if value == string(protocol.CapabilityStdinCancel) {
					sawCapability = true
				}
			}
		}
		if eventType(event) == string(protocol.TypeResult) {
			if details, ok := event.object["details"].(map[string]any); ok && details["controlCommandId"] == commandID {
				sawCommandID = true
			}
		}
	}
	if !sawCapability {
		t.Fatal("hello.capabilities does not advertise stdin.cancel")
	}
	if !sawCommandID {
		t.Fatal("cancel commandId was not echoed in result details")
	}
}

func TestEnvironmentEnsure_SuccessRollsBackToStableState(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "ensure"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			return &m5TestEnvironment{calls: &log.calls}, nil
		}),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want success; stderr=%q", code, stderr.String())
	}
	if len(store.writes) != 0 {
		t.Fatalf("environment writes = %#v, want no durable state change", store.writes)
	}
	statuses := stateStatusesFromEvents(t, stdout.String())
	want := []string{string(protocol.StatePreparingUV), string(protocol.StateEnvironmentBroken)}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("state statuses = %#v, want %#v", statuses, want)
	}
}

func TestEnvironmentEnsure_FailurePersistsActiveRevisionAsBroken(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateReadyToStart,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "ensure"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			return &m5TestEnvironment{calls: &log.calls, uvErr: errors.New("fake uv failure")}, nil
		}),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code == protocol.ExitCodeSuccess {
		t.Fatal("Execute() exit code = success, want uv failure")
	}
	if len(store.writes) != 1 || store.writes[0].Status != protocol.StateEnvironmentBroken {
		t.Fatalf("environment writes = %#v, want one environment_broken state", store.writes)
	}
	statuses := stateStatusesFromEvents(t, stdout.String())
	want := []string{string(protocol.StatePreparingUV), string(protocol.StateEnvironmentBroken)}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("state statuses = %#v, want %#v", statuses, want)
	}
}

func TestEnvironmentRepair_PreservesStableState(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	environment := &m5TestEnvironment{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			return &m5TestWorkspace{calls: &log.calls}, nil
		}),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want success; stderr=%q", code, stderr.String())
	}
	if environment.uvRepairCalls != 1 || environment.pythonPrepareCalls != 1 {
		t.Fatalf("environment repair calls = uv %d Python %d, want 1/1", environment.uvRepairCalls, environment.pythonPrepareCalls)
	}
	if len(store.writes) != 0 {
		t.Fatalf("environment writes = %#v, want no durable state change", store.writes)
	}
	statuses := stateStatusesFromEvents(t, stdout.String())
	want := []string{string(protocol.StatePreparingUV), string(protocol.StateEnvironmentBroken)}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("state statuses = %#v, want %#v", statuses, want)
	}
}

func TestRecoverM5Transaction_FailsClosedForLiveAndRemovesStale(t *testing.T) {
	tests := []struct {
		name      string
		pid       uint32
		wantError protocol.Code
	}{
		{name: "live transaction", pid: uint32(os.Getpid()), wantError: protocol.CodeMutationInProgress},
		{name: "stale transaction", pid: ^uint32(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			appRoot := filepath.Join(root, "app")
			if err := os.MkdirAll(appRoot, 0o700); err != nil {
				t.Fatalf("os.MkdirAll() error = %v", err)
			}
			layout, err := config.NewLayout(appRoot, root)
			if err != nil {
				t.Fatalf("config.NewLayout() error = %v", err)
			}
			store, err := state.NewStore(t.Context(), layout)
			if err != nil {
				t.Fatalf("state.NewStore() error = %v", err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Errorf("store.Close() error = %v", err)
				}
			})
			transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
				OperationID: "01J00000000000000000000001",
				Command:     "environment ensure",
				PID:         test.pid,
				Stage:       protocol.StageUVCheck,
			})
			if err != nil {
				t.Fatalf("NewTransaction() error = %v", err)
			}
			if err := store.WriteTransaction(t.Context(), state.TransactionMutation, transaction); err != nil {
				t.Fatalf("WriteTransaction() error = %v", err)
			}
			err = recoverM5Transaction(t.Context(), store, layout)
			if test.wantError != "" {
				if findOperationErrorCode(err, test.wantError) == nil {
					t.Fatalf("recoverM5Transaction() error = %v, want code %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("recoverM5Transaction() error = %v, want nil", err)
			}
			if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("ReadTransaction() error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestRecoverM5Transaction_InvalidatesReadyState(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.MkdirAll(appRoot, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(app root) error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(repo) error = %v", err)
	}
	if err := os.WriteFile(layout.PythonVersionFile(), []byte("3.12.10\n"), 0o600); err != nil {
		t.Fatalf("write .python-version error = %v", err)
	}
	if err := os.WriteFile(layout.PyProjectFile(), []byte("[project]\nrequires-python = \">=3.12,<3.13\"\n"), 0o600); err != nil {
		t.Fatalf("write pyproject.toml error = %v", err)
	}
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	ready, err := store.NewReadyEnvironment("v5.4.0", "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := store.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatalf("WriteEnvironment() error = %v", err)
	}
	transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID:   "01J00000000000000000000002",
		Command:       "dependencies sync",
		PID:           ^uint32(0),
		TargetVersion: "v5.4.0",
		Stage:         protocol.StageDependenciesSync,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionMutation, transaction); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}

	if err := recoverM5Transaction(t.Context(), store, layout); err != nil {
		t.Fatalf("recoverM5Transaction() error = %v", err)
	}
	got, err := store.ReadEnvironment(t.Context())
	if err != nil {
		t.Fatalf("ReadEnvironment() error = %v", err)
	}
	if got.Status != protocol.StateEnvironmentBroken || got.Broken == nil {
		t.Fatalf("environment state = %#v, want environment_broken", got)
	}
	broken := got.Broken
	if broken.Reason != state.ReasonOperationFailed ||
		broken.Stage != protocol.StageDependenciesSync ||
		broken.PythonVersion != "3.12.10" || broken.UVVersion != uv.FixedVersion {
		t.Fatalf("broken facts = %#v, want stale dependency sync with exact tools", broken)
	}
	if got.LastSuccessful != ready.LastSuccessful {
		t.Fatalf("lastSuccessful = %#v, want %#v", got.LastSuccessful, ready.LastSuccessful)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("ReadTransaction() error = %v, want ErrNotFound", err)
	}
}

func TestRecoverM5Transaction_StateWriteFailureRetainsEvidence(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.MkdirAll(appRoot, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(app root) error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	ready, err := store.NewReadyEnvironment("v5.4.0", "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := store.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatalf("WriteEnvironment() error = %v", err)
	}
	transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID: "01J00000000000000000000003",
		Command:     "environment repair",
		PID:         ^uint32(0),
		Stage:       protocol.StageUVDownload,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionMutation, transaction); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	failing := &failingEnvironmentWriteStore{Store: store, writeErr: errors.New("injected environment write failure")}

	err = recoverM5Transaction(t.Context(), failing, layout)
	if findOperationErrorCode(err, protocol.CodeStateWriteFailed) == nil {
		t.Fatalf("recoverM5Transaction() error = %v, want STATE_WRITE_FAILED", err)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); err != nil {
		t.Fatalf("ReadTransaction() error = %v, want retained transaction", err)
	}
	got, err := store.ReadEnvironment(t.Context())
	if err != nil {
		t.Fatalf("ReadEnvironment() error = %v", err)
	}
	if got.Status != protocol.StateReadyToStart {
		t.Fatalf("environment status = %q, want original ready_to_start", got.Status)
	}
}

func TestRecoverM5Transaction_PreservesWorkspaceOwnedEvidence(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.MkdirAll(appRoot, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(app root) error = %v", err)
	}
	layout, err := config.NewLayout(appRoot, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID:   "01J00000000000000000000004",
		Command:       "workspace sync",
		PID:           ^uint32(0),
		TargetVersion: "v5.4.0",
		Stage:         protocol.StageWorkspaceClone,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionMutation, transaction); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}

	err = recoverM5Transaction(t.Context(), store, layout)
	if findOperationErrorCode(err, protocol.CodeUpdateStateAmbiguous) == nil {
		t.Fatalf("recoverM5Transaction() error = %v, want UPDATE_STATE_AMBIGUOUS", err)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); err != nil {
		t.Fatalf("ReadTransaction() error = %v, want workspace evidence retained", err)
	}
	update, err := store.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID: transaction.OperationID, Command: "workspace sync", PID: transaction.PID,
		TargetVersion: transaction.TargetVersion, Stage: transaction.Stage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionUpdate, update); err != nil {
		t.Fatal(err)
	}
	if err := recoverPreparationTransaction(t.Context(), store, layout, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("mutation = %v, want removed", err)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionUpdate); err != nil {
		t.Fatalf("update ownership lost: %v", err)
	}
}

func TestRepair_StagesFollowExecution(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	store := &m5TestStateStore{
		calls:              &log.calls,
		recordTransactions: true,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.3.0",
				Commit:  "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			},
		},
	}
	environment := &m5TestEnvironment{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	code := Execute(
		t.Context(),
		[]string{"--app-root", root, "--output", "ndjson", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return &m5TestWorkspace{calls: &log.calls}, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("repair exit code = %d, want success; stderr=%q", code, stderr.String())
	}
	wantCalls := []string{
		"acquire",
		"transaction:repair",
		"transaction:uv.download",
		"uv-repair",
		"transaction:uv.verify",
		"transaction:python.check",
		"python-spec",
		"transaction:python.install",
		"python",
		"transaction:dependencies.rebuild",
		"dependencies-rebuild",
		"transaction:dependencies.sync",
		"dependencies",
		"ready",
		"logger-close",
		"lease-close",
		"coordinator-close",
		"store-close",
	}
	if !reflect.DeepEqual(log.calls, wantCalls) {
		t.Fatalf("repair calls = %#v, want %#v", log.calls, wantCalls)
	}
	if !environment.pythonRequest.Reinstall {
		t.Fatal("repair PythonRequest.Reinstall = false, want true")
	}
}

func TestM5Failure_PersistsKnownToolVersions(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateReadyToStart,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	environment := &m5TestEnvironment{calls: &log.calls, pythonErr: errors.New("fake Python reinstall failure")}
	var stdout, stderr bytes.Buffer
	code := Execute(
		t.Context(),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return &m5TestWorkspace{calls: &log.calls}, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code == protocol.ExitCodeSuccess {
		t.Fatal("environment repair exit code = success, want Python failure")
	}
	if len(store.writes) != 1 || store.writes[0].Broken == nil {
		t.Fatalf("environment writes = %#v, want one broken state", store.writes)
	}
	broken := store.writes[0].Broken
	if broken.Stage != protocol.StagePythonInstall ||
		broken.PythonVersion != "3.12.10" || broken.UVVersion != uv.FixedVersion {
		t.Fatalf("broken facts = %#v, want exact Python and uv versions", broken)
	}
}

func TestRepair_DoesNotTouchProtectedInputs(t *testing.T) {
	root := t.TempDir()
	protected := map[string][]byte{
		"config/settings.json": []byte("config"),
		"data/user.db":         []byte("data"),
		"history/run.log":      []byte("history"),
		"script/custom.py":     []byte("script"),
		"debug/trace.log":      []byte("debug"),
		"plugins/plugin.txt":   []byte("plugin"),
	}
	for relative, payload := range protected {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls}
	workspace := &m5TestWorkspace{calls: &log.calls}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	coordinator := &m5TestCoordinator{calls: &log.calls}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return coordinator, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("repair exit code = %d, want success; stderr=%q", code, stderr.String())
	}
	if environment.uvRepairCalls != 1 || environment.pythonPrepareCalls != 1 ||
		environment.rebuildCalls != 1 || environment.syncCalls != 1 ||
		len(store.writes) != 1 || store.writes[0].Status != protocol.StateReadyToStart {
		t.Fatalf(
			"repair calls=uv:%d Python:%d rebuild:%d sync:%d state writes=%#v, want one complete repair and ready state",
			environment.uvRepairCalls,
			environment.pythonPrepareCalls,
			environment.rebuildCalls,
			environment.syncCalls,
			store.writes,
		)
	}
	for relative, want := range protected {
		path := filepath.Join(root, relative)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", relative, err)
		}
		if string(got) != string(want) {
			t.Errorf("protected file %q = %q, want %q", relative, got, want)
		}
	}
}

func TestRepair_RecoversBrokenEnvironment(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.3.0",
				Commit:  "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return &m5TestWorkspace{calls: &log.calls}, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("repair exit code = %d, want success; stderr=%q", code, stderr.String())
	}
	if len(store.writes) != 1 || store.writes[0].Status != protocol.StateReadyToStart {
		t.Fatalf("state writes = %#v, want ready_to_start", store.writes)
	}
	ready := store.writes[0].LastSuccessful
	if ready.Version != "v5.4.0" || ready.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("last successful = %#v, want active workspace revision", ready)
	}
}

func TestRepair_FailurePersistsBrokenEnvironment(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls, repairErr: errors.New("fake repair failure")}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateReadyToStart,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "repair"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return &m5TestWorkspace{calls: &log.calls}, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
	)
	if code == protocol.ExitCodeSuccess {
		t.Fatal("repair exit code = success, want repair failure")
	}
	if len(store.writes) != 1 || store.writes[0].Status != protocol.StateEnvironmentBroken {
		t.Fatalf("state writes = %#v, want one environment_broken write", store.writes)
	}
	if store.writes[0].Broken == nil || store.writes[0].Broken.Reason != state.ReasonOperationFailed {
		t.Fatalf("broken state = %#v, want operation_failed", store.writes[0].Broken)
	}
}

func stateStatusesFromEvents(t *testing.T, payload string) []string {
	t.Helper()
	events := parseNDJSON(t, payload)
	statuses := make([]string, 0)
	for _, event := range events {
		if eventType(event) == string(protocol.TypeState) {
			statuses = append(statuses, eventString(event, "status"))
		}
	}
	return statuses
}

type m5TestEnvironment struct {
	calls                  *[]string
	uvErr                  error
	dependencyErr          error
	repairErr              error
	pythonErr              error
	rebuildErr             error
	dependencyCheckEnabled bool
	waitForCancel          bool
	syncCalls              int
	readyCalls             int
	repairCalls            int
	uvRepairCalls          int
	pythonPrepareCalls     int
	rebuildCalls           int
	uvPolicy               mirror.Policy
	pythonRequest          uv.PythonRequest
	dependencyRequest      uv.DependenciesRequest
	uvProgress             []mirror.DownloadProgress
}

func (s *m5TestEnvironment) Ensure(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, errors.New("not used")
}

func (s *m5TestEnvironment) Check(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, errors.New("not used")
}

func (s *m5TestEnvironment) Repair(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	s.repairCalls++
	*s.calls = append(*s.calls, "repair")
	if s.repairErr != nil {
		return uv.EnvironmentResult{}, s.repairErr
	}
	return uv.EnvironmentResult{
		UVExecutable: "uv.exe",
		Python:       uv.PythonSpec{Version: uv.PythonVersion{Major: 3, Minor: 12, Patch: 10}},
		Dependencies: uv.DependenciesResult{Synchronized: true},
	}, nil
}

func (s *m5TestEnvironment) RepairEnvironment(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	s.repairCalls++
	*s.calls = append(*s.calls, "environment-repair")
	if s.repairErr != nil {
		return uv.EnvironmentResult{}, s.repairErr
	}
	return uv.EnvironmentResult{
		UVExecutable: "uv.exe",
		Python:       uv.PythonSpec{Version: uv.PythonVersion{Major: 3, Minor: 12, Patch: 10}},
	}, nil
}

func (s *m5TestEnvironment) EnsureUV(ctx context.Context, _ string, policy mirror.Policy) (string, error) {
	s.uvPolicy = policy
	if s.waitForCancel {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if s.uvErr != nil {
		return "", s.uvErr
	}
	*s.calls = append(*s.calls, "uv")
	return "uv.exe", nil
}

func (s *m5TestEnvironment) EnsureUVWithProgress(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	_ uv.LineFunc,
	progress mirror.ProgressFunc,
) (string, error) {
	for _, update := range s.uvProgress {
		if progress == nil {
			return "", errors.New("test uv progress callback is nil")
		}
		if err := progress(update); err != nil {
			return "", err
		}
	}
	return s.EnsureUV(ctx, operationID, policy)
}

func (s *m5TestEnvironment) RepairUV(context.Context, string, mirror.Policy) (string, error) {
	s.uvRepairCalls++
	*s.calls = append(*s.calls, "uv-repair")
	if s.repairErr != nil {
		return "", s.repairErr
	}
	return "uv.exe", nil
}

func (s *m5TestEnvironment) CheckUV(context.Context) (bool, error) { return true, nil }

func (s *m5TestEnvironment) ReadPythonSpec(context.Context, string) (uv.PythonSpec, error) {
	*s.calls = append(*s.calls, "python-spec")
	return uv.PythonSpec{Version: uv.PythonVersion{Major: 3, Minor: 12, Patch: 10}}, nil
}

func (s *m5TestEnvironment) PreparePython(_ context.Context, request uv.PythonRequest) (uv.PythonResult, error) {
	s.pythonPrepareCalls++
	s.pythonRequest = request
	*s.calls = append(*s.calls, "python")
	if s.pythonErr != nil {
		return uv.PythonResult{}, s.pythonErr
	}
	return uv.PythonResult{Spec: uv.PythonSpec{Version: uv.PythonVersion{Major: 3, Minor: 12, Patch: 10}}}, nil
}

func (s *m5TestEnvironment) CheckPython(context.Context, uv.PythonRequest) (uv.PythonCheckResult, error) {
	return uv.PythonCheckResult{Spec: uv.PythonSpec{Version: uv.PythonVersion{Major: 3, Minor: 12, Patch: 10}}}, nil
}

func (s *m5TestEnvironment) SyncDependencies(ctx context.Context, request uv.DependenciesRequest) (uv.DependenciesResult, error) {
	s.dependencyRequest = request
	s.syncCalls++
	*s.calls = append(*s.calls, "dependencies")
	if request.Attempt != nil {
		// 模拟一次镜像轮换：首选失败后由次选完成，调用方据此发 progress。
		for index, attempt := range m5TestMirrorAttempts {
			if err := request.Attempt(ctx, attempt); err != nil {
				return uv.DependenciesResult{}, fmt.Errorf("report attempt %d: %w", index, err)
			}
		}
	}
	if s.dependencyErr != nil {
		return uv.DependenciesResult{}, s.dependencyErr
	}
	return uv.DependenciesResult{
		LockfileChecked: true,
		Synchronized:    true,
		SourceKind:      "package-index",
		Source:          "tsinghua",
		AttemptCount:    2,
		LockRewritten:   true,
	}, nil
}

// m5TestMirrorAttempts 是假依赖服务回放的镜像尝试序列。
var m5TestMirrorAttempts = []uv.MirrorAttempt{
	{SourceKind: "package-index", Source: "aliyun", SourceTry: 1, GlobalTry: 1},
	{SourceKind: "package-index", Source: "tsinghua", SourceTry: 1, GlobalTry: 2},
}

// TestDependenciesSync_ResultReportsMirrorSource 锁定 C10 第 7 条：
// dependencies sync 的 result.details 报告本次实际使用的包索引源与尝试次数，
// 每次尝试另发一条 progress 供人类查看（progress 没有 details，机器只读 result）。
func TestDependenciesSync_ResultReportsMirrorSource(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls}
	workspace := &m5TestWorkspace{calls: &log.calls}
	store := &m5TestStateStore{
		calls: &log.calls,
		initial: state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "dependencies", "sync"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != int(protocol.ExitCodeSuccess) {
		t.Fatalf("exit code = %d, want 0; stderr=%q; stdout=%q", code, stderr.String(), stdout.String())
	}
	events := parseNDJSON(t, stdout.String())
	result := events[len(events)-1]
	details, ok := result.object["details"].(map[string]any)
	if !ok {
		t.Fatalf("result details = %#v, want object", result.object["details"])
	}
	wants := map[string]any{
		"sourceKind":    "package-index",
		"source":        "tsinghua",
		"attemptCount":  float64(2),
		"lockRewritten": true,
	}
	for key, want := range wants {
		if got := details[key]; got != want {
			t.Errorf("result details[%q] = %#v, want %#v", key, got, want)
		}
	}
	progressMessages := make([]string, 0, len(events))
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) && eventString(event, "stage") == string(protocol.StageDependenciesSync) {
			progressMessages = append(progressMessages, eventString(event, "message"))
		}
	}
	for _, attempt := range m5TestMirrorAttempts {
		found := false
		for _, message := range progressMessages {
			if strings.Contains(message, attempt.Source) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no dependencies.sync progress mentions source %q; got %#v", attempt.Source, progressMessages)
		}
	}
}

func (s *m5TestEnvironment) CheckDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error) {
	if !s.dependencyCheckEnabled {
		return uv.DependenciesResult{}, errors.New("not used")
	}
	return uv.DependenciesResult{LockfileChecked: true, Synchronized: true}, nil
}

func (s *m5TestEnvironment) RebuildDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error) {
	s.rebuildCalls++
	*s.calls = append(*s.calls, "dependencies-rebuild")
	if s.rebuildErr != nil {
		return uv.DependenciesResult{}, s.rebuildErr
	}
	return uv.DependenciesResult{Rebuilt: true}, nil
}

type m5TestWorkspace struct {
	calls      *[]string
	emitStates bool
}

func (s *m5TestWorkspace) Check(context.Context) (gitrepo.CheckResult, error) {
	return gitrepo.CheckResult{
		Healthy: true,
		Version: "v5.4.0",
		Branch:  "release/v5.4.0",
		Commit:  "0123456789abcdef0123456789abcdef01234567",
		Source:  "github",
	}, nil
}

func (s *m5TestWorkspace) Sync(_ context.Context, request gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
	*s.calls = append(*s.calls, "workspace")
	if request.MutationLease == nil {
		return gitrepo.SyncResult{}, errors.New("workspace did not receive shared mutation lease")
	}
	revision, err := gitrepo.NewRevision(
		"v5.4.0",
		"release/v5.4.0",
		"0123456789abcdef0123456789abcdef01234567",
		"github",
	)
	if err != nil {
		return gitrepo.SyncResult{}, err
	}
	if s.emitStates {
		if err := request.Emitter.EmitState(protocol.StateEvent{
			Stage:   protocol.StageWorkspaceSwap,
			Status:  protocol.StateEnvironmentBroken,
			Message: "workspace internal state",
		}); err != nil {
			return gitrepo.SyncResult{}, err
		}
		if err := request.Emitter.EmitState(protocol.StateEvent{
			Stage:   protocol.StageWorkspaceCheck,
			Status:  protocol.StateReadyToStart,
			Message: "workspace no-op state",
		}); err != nil {
			return gitrepo.SyncResult{}, err
		}
	}
	return gitrepo.SyncResult{Revision: revision, Changed: true, Status: protocol.StateEnvironmentBroken}, nil
}

type m5TestStateStore struct {
	writes             []state.EnvironmentState
	calls              *[]string
	initial            state.EnvironmentState
	transaction        state.TransactionState
	transactionActive  bool
	recordTransactions bool
	removeCalls        int
	removeContextErr   error
	removeErr          error
}

type failingEnvironmentWriteStore struct {
	*state.Store
	writeErr error
}

func (s *failingEnvironmentWriteStore) WriteEnvironment(context.Context, state.EnvironmentState) error {
	return s.writeErr
}

func (s *m5TestStateStore) ReadEnvironment(context.Context) (state.EnvironmentState, error) {
	if len(s.writes) == 0 {
		if s.initial.Status != "" {
			return s.initial, nil
		}
		return state.EnvironmentState{}, state.ErrNotFound
	}
	return s.writes[len(s.writes)-1], nil
}

func (s *m5TestStateStore) NewReadyEnvironment(version, commit string) (state.EnvironmentState, error) {
	*s.calls = append(*s.calls, "ready")
	return state.EnvironmentState{Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: version, Commit: commit}}, nil
}

func (s *m5TestStateStore) NewBrokenEnvironment(last state.Revision, broken state.BrokenEnvironment) (state.EnvironmentState, error) {
	return state.EnvironmentState{Status: protocol.StateEnvironmentBroken, LastSuccessful: last, Broken: &broken}, nil
}

func (s *m5TestStateStore) WriteEnvironment(_ context.Context, value state.EnvironmentState) error {
	s.writes = append(s.writes, value)
	return nil
}

func (s *m5TestStateStore) NewTransaction(_ state.TransactionKind, input state.TransactionInput) (state.TransactionState, error) {
	s.transaction = state.TransactionState{
		SchemaVersion: state.SchemaVersion,
		OperationID:   input.OperationID,
		Command:       input.Command,
		PID:           input.PID,
		StartedAt:     time.Now().UTC(),
		TargetVersion: input.TargetVersion,
		Stage:         input.Stage,
	}
	return s.transaction, nil
}

func (s *m5TestStateStore) WriteTransaction(_ context.Context, _ state.TransactionKind, value state.TransactionState) error {
	s.transaction = value
	s.transactionActive = true
	if s.recordTransactions {
		*s.calls = append(*s.calls, "transaction:"+string(value.Stage))
	}
	return nil
}

func (s *m5TestStateStore) ReadTransaction(context.Context, state.TransactionKind) (state.TransactionSnapshot, error) {
	if !s.transactionActive {
		return state.TransactionSnapshot{}, state.ErrNotFound
	}
	return state.TransactionSnapshot{}, nil
}

func (s *m5TestStateStore) RemoveTransaction(ctx context.Context, _ state.TransactionSnapshot) error {
	s.removeCalls++
	s.removeContextErr = ctx.Err()
	if s.removeErr != nil {
		return s.removeErr
	}
	s.transactionActive = false
	return nil
}

func (s *m5TestStateStore) RemoveOwnedTransaction(
	ctx context.Context,
	_ state.TransactionKind,
	expected state.TransactionState,
) error {
	s.removeCalls++
	s.removeContextErr = ctx.Err()
	if !s.transactionActive {
		return state.ErrNotFound
	}
	if s.transaction != expected {
		return state.ErrTransactionChanged
	}
	if s.removeErr != nil {
		return s.removeErr
	}
	s.transactionActive = false
	return nil
}

func (s *m5TestStateStore) Close() error {
	*s.calls = append(*s.calls, "store-close")
	return nil
}

type m5TestCoordinator struct{ calls *[]string }

func (s *m5TestCoordinator) AcquireMutation(context.Context) (gitrepo.MutationLease, error) {
	*s.calls = append(*s.calls, "acquire")
	return m5TestLease{calls: s.calls}, nil
}

func (s *m5TestCoordinator) Close() error {
	*s.calls = append(*s.calls, "coordinator-close")
	return nil
}

type m5TestLease struct{ calls *[]string }

func (s m5TestLease) Close() error {
	*s.calls = append(*s.calls, "lease-close")
	return nil
}

type m5TestLog struct {
	calls []string
}

func (s *m5TestLog) LogPath() string { return "" }
func (s *m5TestLog) Close() error {
	s.calls = append(s.calls, "logger-close")
	return nil
}
func (s *m5TestLog) Record(context.Context, logging.Level, string, map[string]any) (logging.WriteResult, error) {
	return logging.WriteResult{}, nil
}

func legacyM5Options() []Option {
	var calls []string
	unsupportedEnvironment := &m5UnsupportedEnvironment{}
	unsupportedWorkspace := &m5UnsupportedWorkspace{}
	return []Option{
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			return unsupportedEnvironment, nil
		}),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			return unsupportedWorkspace, nil
		}),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return &m5TestStateStore{calls: &calls}, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &calls}, nil
		}),
	}
}

type m5UnsupportedEnvironment struct{}

func (m5UnsupportedEnvironment) Ensure(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, notImplementedError{stage: protocol.StageBootstrap}
}
func (m5UnsupportedEnvironment) Check(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, notImplementedError{stage: protocol.StageUVCheck}
}
func (m5UnsupportedEnvironment) Repair(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, notImplementedError{stage: protocol.StageRepair}
}
func (m5UnsupportedEnvironment) RepairEnvironment(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error) {
	return uv.EnvironmentResult{}, notImplementedError{stage: protocol.StageRepair}
}
func (m5UnsupportedEnvironment) EnsureUV(context.Context, string, mirror.Policy) (string, error) {
	return "", notImplementedError{stage: protocol.StageUVCheck}
}
func (m5UnsupportedEnvironment) RepairUV(context.Context, string, mirror.Policy) (string, error) {
	return "", notImplementedError{stage: protocol.StageUVCheck}
}
func (m5UnsupportedEnvironment) CheckUV(context.Context) (bool, error) {
	return false, notImplementedError{stage: protocol.StageUVCheck}
}
func (m5UnsupportedEnvironment) ReadPythonSpec(context.Context, string) (uv.PythonSpec, error) {
	return uv.PythonSpec{}, notImplementedError{stage: protocol.StagePythonCheck}
}
func (m5UnsupportedEnvironment) PreparePython(context.Context, uv.PythonRequest) (uv.PythonResult, error) {
	return uv.PythonResult{}, notImplementedError{stage: protocol.StagePythonCheck}
}
func (m5UnsupportedEnvironment) CheckPython(context.Context, uv.PythonRequest) (uv.PythonCheckResult, error) {
	return uv.PythonCheckResult{}, notImplementedError{stage: protocol.StagePythonCheck}
}
func (m5UnsupportedEnvironment) SyncDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error) {
	return uv.DependenciesResult{}, notImplementedError{stage: protocol.StageDependenciesSync}
}
func (m5UnsupportedEnvironment) CheckDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error) {
	return uv.DependenciesResult{}, notImplementedError{stage: protocol.StageDependenciesCheck}
}
func (m5UnsupportedEnvironment) RebuildDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error) {
	return uv.DependenciesResult{}, notImplementedError{stage: protocol.StageDependenciesRebuild}
}

type m5UnsupportedWorkspace struct{}

func (m5UnsupportedWorkspace) Check(context.Context) (gitrepo.CheckResult, error) {
	return gitrepo.CheckResult{}, notImplementedError{stage: protocol.StageWorkspaceCheck}
}
func (m5UnsupportedWorkspace) Sync(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
	return gitrepo.SyncResult{}, notImplementedError{stage: protocol.StageWorkspaceClone}
}
