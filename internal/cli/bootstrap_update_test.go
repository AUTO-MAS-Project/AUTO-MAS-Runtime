package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestBootstrap_AlphaStartupAndExplicitUpdate(t *testing.T) {
	for _, startup := range []bool{true, false} {
		name := "explicit update"
		if startup {
			name = "startup"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			log := &m5TestLog{}
			environment := &m5TestEnvironment{calls: &log.calls, dependencyCheckEnabled: true}
			layout, err := config.NewLayout(root, root)
			if err != nil {
				t.Fatal(err)
			}
			writeBootstrapFile(t, layout.VenvConfigFile())
			writeBootstrapFile(t, layout.VenvPythonExecutable())
			version := "v5.6.0-alpha.123"
			commit := strings.Repeat("a", 40)
			store := &m5TestStateStore{calls: &log.calls, initial: state.EnvironmentState{
				Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: version, Commit: commit},
			}}
			called := false
			workspace := workspaceTestService{sync: func(_ context.Context, req gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				called = true
				if req.UseCurrent != startup || req.Target.Branch() != "dev" {
					t.Fatalf("alpha bootstrap UseCurrent=%t, branch=%q, want %t/dev", req.UseCurrent, req.Target.Branch(), startup)
				}
				revision, err := gitrepo.NewRevision(version, "dev", commit, "github")
				return gitrepo.SyncResult{Revision: revision, Status: protocol.StateReadyToStart}, err
			}}
			args := []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", version}
			if startup {
				args = append(args, "--if-needed")
			}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), args, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
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
			if code != 0 || !called {
				t.Fatalf("alpha bootstrap exit=%d, called=%t, output=%s, stderr=%s", code, called, stdout.String(), stderr.String())
			}
		})
	}
}

func TestBootstrap_IfNeededSkipsOnlyUnchangedReadyEnvironment(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "updated"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			log := &m5TestLog{}
			environment := &m5TestEnvironment{calls: &log.calls, dependencyCheckEnabled: true}
			// 「环境已就绪」现在还要求磁盘上真有一个结构完整的 venv，夹具据此补齐。
			readyLayout, layoutErr := config.NewLayout(root, root)
			if layoutErr != nil {
				t.Fatalf("NewLayout() error = %v", layoutErr)
			}
			writeBootstrapFile(t, readyLayout.VenvPythonExecutable())
			writeBootstrapFile(t, readyLayout.VenvConfigFile())
			commit := strings.Repeat("a", 40)
			store := &m5TestStateStore{calls: &log.calls, initial: state.EnvironmentState{
				Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: "v5.4.0", Commit: commit},
			}}
			workspace := workspaceTestService{sync: func(_ context.Context, req gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				if !req.UseCurrent {
					t.Error("startup sync must prefer current workspace")
				}
				status := protocol.StateReadyToStart
				if changed {
					status = protocol.StateEnvironmentBroken
					commit = strings.Repeat("b", 40)
				}
				revision, err := gitrepo.NewRevision("v5.4.0", "release/v5.4.0", commit, "github")
				return gitrepo.SyncResult{Revision: revision, Changed: changed, Status: status}, err
			}}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0", "--if-needed"},
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
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
			if code != 0 {
				t.Fatalf("bootstrap exit=%d, output=%s, stderr=%s", code, stdout.String(), stderr.String())
			}
			called := strings.Contains(strings.Join(log.calls, ","), "dependencies")
			if called != changed {
				t.Fatalf("calls=%v, want dependency sync=%t", log.calls, changed)
			}
		})
	}
}

// TestBootstrap_IfNeededRebuildsWhenVenvIsBroken 钉住真机事故：状态文件仍是 ready、
// 仓库也没变，但磁盘上的 venv 已经缺了 pyvenv.cfg。此前 --if-needed 只看状态就宣布
// 「当前运行环境已就绪」，把一个一启动就退出的 venv 交给 supervise，用户点多少次重试
// 都不会重建。现在就绪判据必须带上文件系统实证。
func TestBootstrap_IfNeededRebuildsWhenVenvIsBroken(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls}
	commit := strings.Repeat("a", 40)
	store := &m5TestStateStore{calls: &log.calls, initial: state.EnvironmentState{
		Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: "v5.4.0", Commit: commit},
	}}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	// 只铺解释器，故意不铺 pyvenv.cfg——这正是真机残骸的形态。
	writeBootstrapFile(t, layout.VenvPythonExecutable())

	workspace := workspaceTestService{sync: func(_ context.Context, _ gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
		revision, err := gitrepo.NewRevision("v5.4.0", "release/v5.4.0", commit, "github")
		return gitrepo.SyncResult{Revision: revision, Changed: false, Status: protocol.StateReadyToStart}, err
	}}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0", "--if-needed"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
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
	if code != 0 {
		t.Fatalf("bootstrap exit=%d, output=%s, stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(strings.Join(log.calls, ","), "dependencies") {
		t.Fatalf("calls=%v, want dependency sync because the venv is incomplete", log.calls)
	}
}

func TestBootstrap_IfNeededChecksDependencyReadiness(t *testing.T) {
	unsynchronized := &commandError{
		code: protocol.CodeDependencySyncFailed, stage: protocol.StageDependenciesCheck,
		message: "主项目依赖环境未同步", details: map[string]any{"exitCode": 1},
	}
	interpreterMissing := &commandError{
		code: protocol.CodeDependencySyncFailed, stage: protocol.StageDependenciesCheck,
		details: map[string]any{"exitCode": 103},
		cause:   &commandError{code: protocol.CodeUVExecFailed, stage: protocol.StageDependenciesCheck, details: map[string]any{"failureKind": "nonzero_exit"}},
	}
	tests := []struct {
		name     string
		result   uv.DependenciesResult
		err      error
		offline  bool
		wantSync bool
		wantCode protocol.Code
	}{
		{name: "healthy", result: uv.DependenciesResult{LockfileChecked: true, Synchronized: true}},
		{name: "healthy offline", result: uv.DependenciesResult{LockfileChecked: true, Synchronized: true}, offline: true},
		{name: "base interpreter missing", err: interpreterMissing, wantSync: true},
		{name: "packages missing", err: unsynchronized, wantSync: true},
		{name: "packages missing offline", err: unsynchronized, offline: true, wantSync: true},
		{name: "unknown check result", wantSync: true},
		{name: "cancelled", err: context.Canceled, wantCode: protocol.CodeOperationCancelled},
		{name: "uv spawn failed", err: &commandError{code: protocol.CodeUVExecFailed, stage: protocol.StageDependenciesCheck}, wantCode: protocol.CodeUVExecFailed},
		{name: "output failed", err: &commandError{code: protocol.CodeOutputWriteFailed, stage: protocol.StageDependenciesCheck}, wantCode: protocol.CodeOutputWriteFailed},
		{name: "wrapped output failed", err: &commandError{
			code: protocol.CodeDependencySyncFailed, stage: protocol.StageDependenciesCheck, details: map[string]any{"exitCode": 1},
			cause: &commandError{code: protocol.CodeOutputWriteFailed, stage: protocol.StageDependenciesCheck},
		}, wantCode: protocol.CodeOutputWriteFailed},
		{name: "wrapped stream failure", err: &commandError{
			code: protocol.CodeDependencySyncFailed, stage: protocol.StageDependenciesCheck, details: map[string]any{"exitCode": 1},
			cause: &commandError{code: protocol.CodeUVExecFailed, stage: protocol.StageDependenciesCheck, details: map[string]any{"failureKind": "output_read_failed"}},
		}, wantCode: protocol.CodeDependencySyncFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, root)
			if err != nil {
				t.Fatal(err)
			}
			writeBootstrapFile(t, layout.VenvConfigFile())
			writeBootstrapFile(t, layout.VenvPythonExecutable())
			log := &m5TestLog{}
			base := &m5TestEnvironment{calls: &log.calls}
			environment := &bootstrapReadinessEnvironment{m5TestEnvironment: base}
			commit := strings.Repeat("a", 40)
			checked := false
			environment.check = func(_ context.Context, request uv.DependenciesRequest) (uv.DependenciesResult, error) {
				checked = true
				if !request.MirrorPolicy.Offline() || request.ProjectDir != layout.RepoDir() || request.ProjectEnvDir != layout.VenvDir() ||
					request.PythonVersion != "3.12.10" || request.Commit != commit || request.Branch != "release/v5.4.0" || request.OperationID == "" || request.Line == nil {
					t.Fatalf("check request = %+v, want offline managed identity and operation log", request)
				}
				return test.result, test.err
			}
			store := &m5TestStateStore{calls: &log.calls, initial: state.EnvironmentState{
				Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: "v5.4.0", Commit: commit},
			}}
			workspace := workspaceTestService{sync: func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				revision, err := gitrepo.NewRevision("v5.4.0", "release/v5.4.0", commit, "github")
				return gitrepo.SyncResult{Revision: revision, Status: protocol.StateReadyToStart}, err
			}}
			args := []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0", "--if-needed"}
			if test.offline {
				args = append([]string{"--offline"}, args...)
			} else {
				args = append([]string{"--mirror", "package-index=aliyun"}, args...)
			}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), args, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
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
			if !checked {
				t.Fatal("dependency check was not called")
			}
			if test.wantCode != "" {
				events := parseNDJSON(t, stdout.String())
				last := events[len(events)-1]
				if code == 0 || last.object["code"] != string(test.wantCode) {
					t.Fatalf("exit=%d, result=%v, want %s; stderr=%s", code, last.object, test.wantCode, stderr.String())
				}
			} else if code != 0 {
				t.Fatalf("exit=%d, output=%s, stderr=%s", code, stdout.String(), stderr.String())
			}
			if got := base.syncCalls > 0; got != test.wantSync {
				t.Fatalf("sync calls=%d, want sync=%t", base.syncCalls, test.wantSync)
			}
			if !test.wantSync && (base.pythonPrepareCalls != 0 || len(store.writes) != 0) {
				t.Fatalf("python calls=%d, state writes=%v, want no preparation", base.pythonPrepareCalls, store.writes)
			}
			if test.wantSync {
				if base.pythonPrepareCalls != 1 || len(store.writes) != 1 || store.writes[0].Status != protocol.StateReadyToStart {
					t.Fatalf("python calls=%d, writes=%v, want prepared ready environment", base.pythonPrepareCalls, store.writes)
				}
				if base.dependencyRequest.MirrorPolicy.Offline() != test.offline {
					t.Fatal("sync did not preserve original offline policy")
				}
				if !test.offline {
					if preferred, ok := base.dependencyRequest.MirrorPolicy.Preferred(mirror.KindPackageIndex); !ok || preferred != "aliyun" {
						t.Fatal("sync did not preserve mirror preference")
					}
				}
			}
		})
	}
}

type bootstrapReadinessEnvironment struct {
	*m5TestEnvironment
	check func(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error)
}

func (s *bootstrapReadinessEnvironment) CheckDependencies(ctx context.Context, request uv.DependenciesRequest) (uv.DependenciesResult, error) {
	return s.check(ctx, request)
}

func writeBootstrapFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// TestBootstrap_FailureIsRecordedInOperationLog 使用真实日志器验证收尾后失败仍能落盘。
func TestBootstrap_FailureIsRecordedInOperationLog(t *testing.T) {
	root := t.TempDir()
	var calls []string
	environment := &m5TestEnvironment{calls: &calls, uvErr: &commandError{
		code: protocol.CodeUVExecFailed, stage: protocol.StageUVVerify, message: "uv 准备失败",
		details: map[string]any{"failureKind": "start_failed", "windowsError": uint64(5)},
	}}
	store := &m5TestStateStore{calls: &calls}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &calls}, nil
		}),
	)
	if code != protocol.ExitCodeEnvironmentFailure {
		t.Fatalf("bootstrap exit=%d, want UV_EXEC_FAILED exit; output=%s", code, stdout.String())
	}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(layout.RuntimeLogDir(), "bootstrap-*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("bootstrap log paths = %v, error = %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var failures []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry["level"] == "error" {
			failures = append(failures, entry)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("error records = %d, want 1; log=%s", len(failures), data)
	}
	failure := failures[0]
	details, ok := failure["details"].(map[string]any)
	if !ok || details["code"] != "UV_EXEC_FAILED" || details["stage"] != "uv.verify" ||
		details["failureKind"] != "start_failed" || details["windowsError"] != float64(5) {
		t.Fatalf("failure details = %#v, want uv execution diagnostics", failure["details"])
	}
	events := parseNDJSON(t, stdout.String())
	if failure["command"] != "bootstrap" || failure["operationId"] != events[0].object["operationId"] {
		t.Fatalf("failure identity = %#v, want bootstrap and current operationId", failure)
	}
}

func TestRecordOperationFailure_StateWriteCauseStaysInLocalLog(t *testing.T) {
	log := &m5TestLog{}
	binding := &workspaceLogBinding{}
	binding.Set(log)
	deps := &deps{ctx: t.Context(), opLog: binding}
	cause := stateStoreError(protocol.StageBootstrap,
		errors.Join(filesystem.ErrPOSIXUnlinkUnsupported, errors.New("probe rejected")))
	recordOperationFailure(deps, protocol.CodeStateWriteFailed, protocol.StageBootstrap,
		"环境状态写入失败", map[string]any{}, cause)
	if len(log.records) != 1 || !strings.Contains(log.records[0].details["cause"].(string),
		"filesystem POSIX unlink is unsupported") {
		t.Fatalf("state failure log records = %+v, want filesystem cause", log.records)
	}
}

func TestRecordOperationFailure_EarlyStateFailureWritesCauseToStderr(t *testing.T) {
	var stderr bytes.Buffer
	deps := &deps{ctx: t.Context(), io: IO{Err: &stderr}, opLog: &workspaceLogBinding{}}
	cause := stateStoreError(protocol.StageBootstrap,
		errors.Join(filesystem.ErrPOSIXUnlinkUnsupported, errors.New("probe rejected")))
	recordOperationFailure(deps, protocol.CodeStateWriteFailed, protocol.StageBootstrap,
		"环境状态写入失败", map[string]any{}, cause)
	if !strings.Contains(stderr.String(), "filesystem POSIX unlink is unsupported") {
		t.Fatalf("stderr = %q, want underlying filesystem cause", stderr.String())
	}
}
