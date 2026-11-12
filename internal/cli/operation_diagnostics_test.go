package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestWorkspaceSync_FailureDiagnosticsAreRecorded(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "disabled")
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	failure := telemetryDiagnosticError{error: &commandError{code: protocol.CodeGitRepoCleanupFailed, stage: protocol.StageWorkspaceCleanup, message: "仓库清理失败"},
		details: map[string]any{"repository": map[string]any{"component": "previous", "committed": true, "partial": true}}}
	code := Execute(t.Context(), []string{"--app-root", root, "--output", "ndjson", "workspace", "sync", "--version", "v5.8.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, WithCWD(root),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
			return workspaceTestService{sync: func(ctx context.Context, request gitrepo.SyncRequest) (gitrepo.SyncResult, error) {
				logger, err := request.LoggerFactory(ctx, "workspace-sync", request.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if err := logger.Close(); err != nil {
					t.Fatal(err)
				}
				return gitrepo.SyncResult{}, failure
			}}, nil
		}),
	)
	if code != protocol.ExitCodeGitFailure {
		t.Fatalf("exit = %d, want git failure", code)
	}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(layout.RuntimeLogDir(), "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			var row map[string]any
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			if row["level"] != "error" {
				continue
			}
			count++
			details := row["details"].(map[string]any)
			if details["repository"] == nil {
				t.Fatalf("local details = %#v, want repository facts", details)
			}
		}
	}
	if count != 1 {
		t.Fatalf("error records = %d, want 1", count)
	}
	events := parseNDJSON(t, stdout.String())
	if strings.Contains(events[len(events)-1].raw, "repository") {
		t.Fatal("protocol details gained diagnostics")
	}
}
