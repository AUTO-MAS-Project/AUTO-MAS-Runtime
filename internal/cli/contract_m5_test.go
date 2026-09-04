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
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestBootstrapContract(t *testing.T) {
	contracttest.Register(t, "bootstrap", m5ContractRunner("bootstrap", protocol.StageBootstrap, "bootstrap", "--version", "v5.4.0"))
}

func TestEnvironmentCheckContract(t *testing.T) {
	contracttest.Register(t, "environment check", m5ContractRunner("environment check", protocol.StageUVCheck, "environment", "check"))
}

func TestEnvironmentEnsureContract(t *testing.T) {
	contracttest.Register(t, "environment ensure", m5ContractRunner("environment ensure", protocol.StageUVCheck, "environment", "ensure"))
}

func TestEnvironmentRepairContract(t *testing.T) {
	contracttest.Register(t, "environment repair", m5ContractRunner("environment repair", protocol.StageRepair, "environment", "repair"))
}

func TestDependenciesCheckContract(t *testing.T) {
	contracttest.Register(t, "dependencies check", m5ContractRunner("dependencies check", protocol.StageDependenciesCheck, "dependencies", "check"))
}

func TestDependenciesSyncContract(t *testing.T) {
	contracttest.Register(t, "dependencies sync", m5ContractRunner("dependencies sync", protocol.StageDependenciesSync, "dependencies", "sync"))
}

func TestDependenciesRebuildContract(t *testing.T) {
	contracttest.Register(t, "dependencies rebuild", m5ContractRunner("dependencies rebuild", protocol.StageDependenciesRebuild, "dependencies", "rebuild"))
}

func TestRepairContract(t *testing.T) {
	contracttest.Register(t, "repair", m5ContractRunner("repair", protocol.StageRepair, "repair"))
}

func m5ContractRunner(command string, stage protocol.Stage, arguments ...string) contracttest.Runner {
	return func(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
		t.Helper()
		root := t.TempDir()
		log := &m5TestLog{}
		environment := &m5TestEnvironment{calls: &log.calls, dependencyCheckEnabled: true}
		workspace := &m5TestWorkspace{calls: &log.calls}
		store := &m5TestStateStore{
			calls:   &log.calls,
			initial: m5ContractInitialState(command),
		}
		ctx := context.Background()
		environmentFactory := func(*config.Layout) (environmentService, error) {
			return environment, nil
		}
		wantExit := protocol.ExitCodeSuccess
		switch terminal {
		case contracttest.TerminalSuccess:
		case contracttest.TerminalFailure:
			environmentFactory = func(*config.Layout) (environmentService, error) {
				return nil, errors.New("injected M5 contract failure")
			}
			definition, ok := protocol.LookupErrorDefinition(protocol.CodeInternalError)
			if !ok {
				t.Fatal("INTERNAL_ERROR definition is missing")
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

		var stdout, stderr bytes.Buffer
		args := append([]string{"--app-root", root, "--output", "ndjson"}, arguments...)
		code := Execute(
			ctx,
			args,
			IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
			WithCWD(root),
			WithClock(func() time.Time { return time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC) }),
			WithEnvironmentFactory(environmentFactory),
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
		if code != wantExit {
			t.Errorf("exit code = %d, want %d; stderr=%q", code, wantExit, stderr.String())
		}
		assertM5ContractStage(t, terminal, stage, stdout.String())
		assertM5ContractMirrorDetails(t, terminal, command, stdout.String())
		return contracttest.Transcript{Stdout: stdout.Bytes()}
	}
}

func m5ContractInitialState(command string) state.EnvironmentState {
	switch command {
	case "bootstrap", "environment check":
		return state.EnvironmentState{}
	default:
		return state.EnvironmentState{
			Status: protocol.StateEnvironmentBroken,
			LastSuccessful: state.Revision{
				Version: "v5.4.0",
				Commit:  "0123456789abcdef0123456789abcdef01234567",
			},
		}
	}
}

// assertM5ContractMirrorDetails 锁定 C10 第 7 条：凡是执行了 uv sync 的命令，
// 成功 result.details 都必须报告包索引源与尝试次数，字段名与类型不得漂移。
// bootstrap 与 repair 同样跑 SyncDependencies，调用方没有理由在这两条路径上
// 看不到本次实际使用的镜像源。
func assertM5ContractMirrorDetails(
	t *testing.T,
	terminal contracttest.Terminal,
	command string,
	output string,
) {
	t.Helper()
	if terminal != contracttest.TerminalSuccess || !m5CommandReportsMirrorSource(command) {
		return
	}
	events := parseNDJSON(t, output)
	for _, event := range events {
		if eventType(event) != string(protocol.TypeResult) {
			continue
		}
		details, ok := event.object["details"].(map[string]any)
		if !ok {
			t.Fatalf("result details = %#v, want object", event.object["details"])
		}
		if got, ok := details["sourceKind"].(string); !ok || got != mirror.KindPackageIndex.String() {
			t.Errorf("result details[sourceKind] = %#v, want %q", details["sourceKind"], mirror.KindPackageIndex)
		}
		if _, ok := details["source"].(string); !ok {
			t.Errorf("result details[source] = %#v, want string", details["source"])
		}
		if got, ok := details["attemptCount"].(float64); !ok || got < 1 {
			t.Errorf("result details[attemptCount] = %#v, want a positive number", details["attemptCount"])
		}
		if _, ok := details["lockRewritten"].(bool); !ok {
			t.Errorf("result details[lockRewritten] = %#v, want bool", details["lockRewritten"])
		}
		return
	}
	t.Fatal("result event is missing")
}

// m5CommandReportsMirrorSource 列出会执行 uv sync 的命令。environment repair
// 只修 uv 与 Python、不同步依赖，因此不在其中。
func m5CommandReportsMirrorSource(command string) bool {
	switch command {
	case "bootstrap", "repair", "dependencies sync", "dependencies rebuild":
		return true
	default:
		return false
	}
}

func assertM5ContractStage(t *testing.T, terminal contracttest.Terminal, want protocol.Stage, output string) {
	t.Helper()
	if terminal != contracttest.TerminalFailure {
		return
	}
	events := parseNDJSON(t, output)
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError), string(protocol.TypeResult):
			if got := eventString(event, "stage"); got != string(want) {
				t.Errorf("%s stage = %q, want %q", eventType(event), got, want)
			}
		}
	}
}
