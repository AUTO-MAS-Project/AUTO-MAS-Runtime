package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
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
