package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

// TestContract_RegisterVersion 以 contracttest.Register 一行接入通用契约测试。
// failure/cancelled 终态通过 WithVersionSource 注入服务替身，验证共享会话框架。
func TestContract_RegisterVersion(t *testing.T) {
	contracttest.Register(t, "version", versionRunner)
}

func versionRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
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
	switch terminal {
	case contracttest.TerminalSuccess:
		// 使用生产 version.Load。
	case contracttest.TerminalFailure:
		options = append(options, WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{}, errors.New("version source failed")
		}))
	case contracttest.TerminalCancelled:
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ctx = cancelled
		options = append(options, WithVersionSource(func(context.Context) (version.Info, error) {
			return version.Info{}, context.Canceled
		}))
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}
	code := Execute(
		ctx,
		// success 路径固定携带一个 --mirror 值，防止预解析/正式执行
		// 双解析问题再次绕过 Execute 全链路（T3.6 F1 回归护栏）。
		[]string{"--output", "ndjson", "--mirror", "git=github", "version"},
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
		if code != protocol.ExitCodePreconditionFailed {
			t.Errorf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
		}
	case contracttest.TerminalCancelled:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	}
	return contracttest.Transcript{Stdout: stdout.Bytes()}
}
