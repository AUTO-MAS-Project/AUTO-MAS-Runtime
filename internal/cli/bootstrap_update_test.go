package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestBootstrap_IfNeededSkipsOnlyUnchangedReadyEnvironment(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "updated"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			log := &m5TestLog{}
			environment := &m5TestEnvironment{calls: &log.calls}
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

func writeBootstrapFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// TestBootstrap_FailureIsRecordedInOperationLog 钉住「失败必须落盘」：此前 Runtime 的
// 操作日志只透传 uv 输出与清理统计，失败本身、错误码与 stage 一条都不写——真机上取到
// 2468 行日志全是 info，零 warn 零 error，用户报上来也无从查起。
func TestBootstrap_FailureIsRecordedInOperationLog(t *testing.T) {
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls}
	store := &m5TestStateStore{calls: &log.calls}
	workspace := workspaceTestService{sync: func(context.Context, gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
		return gitrepo.SyncResult{}, errors.New("clone failed")
	}}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0"},
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
	if code == 0 {
		t.Fatalf("bootstrap exit=0, want failure; output=%s", stdout.String())
	}
	var failure *m5LogRecord
	for i := range log.records {
		if log.records[i].level == logging.LevelError {
			failure = &log.records[i]
			break
		}
	}
	if failure == nil {
		t.Fatalf("no error-level record; records=%+v", log.records)
	}
	if failure.details["code"] == nil || failure.details["stage"] == nil {
		t.Fatalf("failure record details = %+v, want code and stage", failure.details)
	}
}
