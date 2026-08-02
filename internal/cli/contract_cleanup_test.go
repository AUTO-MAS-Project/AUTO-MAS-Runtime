package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

// TestContract_RegisterCleanup 以 contracttest.Register 一行接入通用契约测试。
// 三个终态都使用真实 cleanup 服务与夹具：失败来自 repo.update 身份不明，
// 取消来自预先取消的 context。
func TestContract_RegisterCleanup(t *testing.T) {
	contracttest.Register(t, "cleanup", cleanupRunner)
}

func cleanupRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	options := []Option{
		WithCWD(t.TempDir()),
		WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
	}
	ctx := context.Background()
	var args []string
	switch terminal {
	case contracttest.TerminalSuccess:
		args = []string{"--output", "ndjson", "--app-root", cleanupFixture(t), "cleanup"}
	case contracttest.TerminalFailure:
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
		args = []string{"--output", "ndjson", "--app-root", root, "cleanup"}
	case contracttest.TerminalCancelled:
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ctx = cancelled
		args = []string{"--output", "ndjson", "--app-root", cleanupFixture(t), "cleanup"}
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}
	code := Execute(
		ctx,
		args,
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		options...,
	)
	switch terminal {
	case contracttest.TerminalSuccess:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case contracttest.TerminalFailure:
		if code != protocol.ExitCodeGitFailure {
			t.Errorf("exit code = %d, want %d", code, protocol.ExitCodeGitFailure)
		}
	case contracttest.TerminalCancelled:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	}
	return contracttest.Transcript{Stdout: stdout.Bytes()}
}
