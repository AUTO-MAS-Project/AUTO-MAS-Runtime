package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func cleanupFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeDoctorFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	writeDoctorFixtureFile(t, filepath.Join(layout.DownloadCacheDir(), "temp.bin"), []byte("y"))
	return root
}

func TestCleanupCommand_NDJSONReport(t *testing.T) {
	t.Parallel()
	root := cleanupFixture(t)
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "--app-root", root, "cleanup"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	events := parseNDJSON(t, stdout.String())
	last := events[len(events)-1]
	if eventType(last) != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", eventType(last))
	}
	if success, ok := last.object["success"].(bool); !ok || !success {
		t.Errorf("result success = %v, want true", last.object["success"])
	}
	details, ok := last.object["details"].(map[string]any)
	if !ok {
		t.Fatalf("result details = %#v, want object", last.object["details"])
	}
	items, ok := details["items"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("details.items = %#v, want at least 2", details["items"])
	}
	summary, ok := details["summary"].(map[string]any)
	if !ok {
		t.Fatalf("details.summary = %#v, want object", details["summary"])
	}
	if summary["cleaned"] != float64(2) {
		t.Errorf("summary = %#v, want cleaned 2", summary)
	}
}

func TestCleanupCommand_HumanOutput(t *testing.T) {
	t.Parallel()
	root := cleanupFixture(t)
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "cleanup"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "HELLO") ||
		!strings.Contains(stdout.String(), "PROGRESS") ||
		!strings.Contains(stdout.String(), "RESULT success=true") {
		t.Errorf("stdout = %q, want human hello/progress/result", stdout.String())
	}
}

func TestCleanupCommand_MutationLockConflictExit70(t *testing.T) {
	t.Parallel()
	root := cleanupFixture(t)
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	heldSet, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		t.Fatalf("lock.NewSet() error = %v", err)
	}
	acquisition, err := heldSet.AcquireMutation(context.Background())
	if err != nil {
		t.Fatalf("AcquireMutation() error = %v", err)
	}
	defer func() {
		if err := acquisition.Lease().Close(); err != nil {
			t.Errorf("lease.Close() error = %v", err)
		}
		if err := heldSet.Close(); err != nil {
			t.Errorf("set.Close() error = %v", err)
		}
	}()

	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "--app-root", root, "cleanup"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
	)
	if code != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationConflict)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent parsedEvent
	for _, event := range events {
		if eventType(event) == string(protocol.TypeError) {
			errorEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeMutationInProgress) {
		t.Errorf("error code = %q, want MUTATION_IN_PROGRESS", got)
	}
}

func TestCleanupCommand_RepoUpdateNoTransactionExit40(t *testing.T) {
	t.Parallel()
	root := cleanupFixture(t)
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	updateDir, err := layout.RepoUpdateDir("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeDoctorFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))

	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "--app-root", root, "cleanup"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
	)
	if code != protocol.ExitCodeGitFailure {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeGitFailure)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent parsedEvent
	for _, event := range events {
		if eventType(event) == string(protocol.TypeError) {
			errorEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeGitRepoCleanupFailed) {
		t.Errorf("error code = %q, want GIT_REPO_CLEANUP_FAILED", got)
	}
	if !dirExists(updateDir) {
		t.Error("fail-closed repo.update directory was removed")
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
