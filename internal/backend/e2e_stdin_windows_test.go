//go:build windows

package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// 本文件用真实的 auto-mas-runtime.exe 黑盒验证增补 1 C13：既有 E2E 直接驱动
// ManagedSupervisor 并自己喂 mailbox，绕过了 CLI 的 stdin reader goroutine，
// 而「宿主断开即关闭」的实现恰恰在那里。

type backendE2EProcessEvents struct {
	mu     sync.Mutex
	events []map[string]any
	closed bool
	wake   chan struct{}
}

func newBackendE2EProcessEvents() *backendE2EProcessEvents {
	return &backendE2EProcessEvents{wake: make(chan struct{})}
}

func (e *backendE2EProcessEvents) append(event map[string]any) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.signalLocked()
	e.mu.Unlock()
}

func (e *backendE2EProcessEvents) close() {
	e.mu.Lock()
	e.closed = true
	e.signalLocked()
	e.mu.Unlock()
}

func (e *backendE2EProcessEvents) signalLocked() {
	close(e.wake)
	e.wake = make(chan struct{})
}

func (e *backendE2EProcessEvents) snapshot() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.events...)
}

// waitFor 等到某条事件满足谓词；stdout 已关闭且仍未出现时失败。
func (e *backendE2EProcessEvents) waitFor(t *testing.T, what string, predicate func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		for _, event := range e.events {
			if predicate(event) {
				e.mu.Unlock()
				return event
			}
		}
		closed := e.closed
		wake := e.wake
		e.mu.Unlock()
		if closed {
			t.Fatalf("stdout closed before %s; events=%#v", what, e.snapshot())
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s; events=%#v", what, e.snapshot())
		}
	}
}

func e2EEventIs(eventType string, status protocol.StateStatus) func(map[string]any) bool {
	return func(event map[string]any) bool {
		return event["type"] == eventType && event["status"] == string(status)
	}
}

// backendE2ERuntimeProcess 是一次真实 exe 的 backend supervise 会话。
type backendE2ERuntimeProcess struct {
	command    *exec.Cmd
	stdin      io.WriteCloser
	stdoutRead *os.File
	stderr     bytes.Buffer
	events     *backendE2EProcessEvents
	waitErr    chan error
}

// startBackendE2ERuntime 构建并启动真实 Runtime，以 development 模式监督夹具后端。
// stdout 用测试自建的管道，读端由测试持有，因此可以在中途关闭以模拟宿主管道失效。
func startBackendE2ERuntime(t *testing.T, fixture *backendE2EFixture) *backendE2ERuntimeProcess {
	t.Helper()
	executable := buildE2EFixture(t, filepath.Join("..", "..", "cmd", "auto-mas-runtime"), fixture.root, "auto-mas-runtime.exe")
	if err := waitE2EPortClosed(t.Context(), fixture.port); err != nil {
		t.Fatalf("port %d is occupied before supervise: %v", fixture.port, err)
	}
	command := exec.Command(executable,
		"--app-root", fixture.appRoot,
		"--output", "ndjson",
		"backend", "supervise",
		"--mode", "development",
		"--repo", fixture.repo,
		"--port", strconv.Itoa(fixture.port),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	command.Stdout = stdoutWrite
	process := &backendE2ERuntimeProcess{
		command:    command,
		stdin:      stdin,
		stdoutRead: stdoutRead,
		events:     newBackendE2EProcessEvents(),
		waitErr:    make(chan error, 1),
	}
	command.Stderr = &process.stderr
	if err := command.Start(); err != nil {
		_ = stdoutWrite.Close()
		_ = stdoutRead.Close()
		t.Fatalf("start runtime: %v", err)
	}
	// 子进程已持有写端副本，父进程这一份必须立刻关闭，否则读端永远等不到 EOF。
	if err := stdoutWrite.Close(); err != nil {
		t.Fatalf("close stdout write end: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-process.waitErr:
		default:
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-process.waitErr
		}
		_ = stdoutRead.Close()
	})
	go func() {
		scanner := bufio.NewScanner(stdoutRead)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var event map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				process.events.append(map[string]any{"type": "unparseable", "raw": scanner.Text(), "error": err.Error()})
				continue
			}
			process.events.append(event)
		}
		process.events.close()
	}()
	go func() { process.waitErr <- command.Wait() }()
	return process
}

// waitExit 等待 Runtime 退出并返回退出码；超时即失败——C13 的核心断言正是「会退出」。
func (p *backendE2ERuntimeProcess) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case err := <-p.waitErr:
		// 结果放回，供 Cleanup 判断进程已经收口。
		p.waitErr <- err
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exitErr):
			return exitErr.ExitCode()
		default:
			t.Fatalf("Wait() error = %v", err)
			return -1
		}
	case <-time.After(timeout):
		t.Fatalf("runtime did not exit within %s after stdin EOF; events=%#v; stderr=%q", timeout, p.events.snapshot(), p.stderr.String())
		return -1
	}
}

func (p *backendE2ERuntimeProcess) closeStdin(t *testing.T) {
	t.Helper()
	if err := p.stdin.Close(); err != nil {
		t.Fatalf("close runtime stdin: %v", err)
	}
}

// TestBackendE2E_StdinEOFShutsDownGracefully 是增补 1 C13 的主用例：真实 exe 到达
// running 后关闭 stdin，Runtime 必须像收到 shutdown 一样优雅关闭后端并以 0 退出，
// result.status=stopped 且不回显 controlCommandId，端口、Mutex 与事务无残留。
func TestBackendE2E_StdinEOFShutsDownGracefully(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{Events: e2EOutputEvents("eof")})
	process := startBackendE2ERuntime(t, fixture)
	running := process.events.waitFor(t, "running state", e2EEventIs("state", protocol.StateRunning))
	details, _ := running["details"].(map[string]any)
	if got, want := details["baseUrl"], "http://127.0.0.1:"+strconv.Itoa(fixture.port); got != want {
		t.Fatalf("running baseUrl = %#v, want %q", got, want)
	}
	pythonPID := waitE2EPIDFile(t, fixture.config.PIDFile)

	process.closeStdin(t)

	if code := process.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("runtime exit code = %d, want 0; stderr=%q; events=%#v", code, process.stderr.String(), process.events.snapshot())
	}
	process.events.waitFor(t, "stopping_backend state", e2EEventIs("state", protocol.StateStoppingBackend))
	process.events.waitFor(t, "stopped state", e2EEventIs("state", protocol.StateStopped))
	result := process.events.waitFor(t, "result", func(event map[string]any) bool { return event["type"] == "result" })
	if got := result["status"]; got != string(protocol.StateStopped) {
		t.Fatalf("result status = %#v, want stopped; result=%#v", got, result)
	}
	if resultDetails, ok := result["details"].(map[string]any); ok {
		if _, exists := resultDetails["controlCommandId"]; exists {
			t.Fatalf("result details = %#v, want no controlCommandId for an implicit shutdown", resultDetails)
		}
	}
	for _, event := range process.events.snapshot() {
		if event["type"] == "warning" && event["code"] == string(protocol.CodeBackendForceTerminated) {
			t.Fatalf("implicit shutdown emitted %s: %#v", protocol.CodeBackendForceTerminated, event)
		}
		if event["type"] == "unparseable" {
			t.Fatalf("stdout carried a non-NDJSON line: %#v", event)
		}
	}
	waitE2EPIDExit(t, pythonPID)
	fixture.assertResourcesReleased(t)
}

// TestBackendE2E_StdinEOFWithBrokenStdoutStillExits 模拟宿主崩溃的真实形态：stdout 管道
// 先失效、随后 stdin EOF。Runtime 写不出任何事件，也必须在有限时间内退出并收口后端进程树、
// 端口、Mutex 与事务；退出码不作要求。
func TestBackendE2E_StdinEOFWithBrokenStdoutStillExits(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{Events: e2EOutputEvents("broken")})
	process := startBackendE2ERuntime(t, fixture)
	process.events.waitFor(t, "running state", e2EEventIs("state", protocol.StateRunning))
	pythonPID := waitE2EPIDFile(t, fixture.config.PIDFile)

	// 先断 stdout（宿主那端的读端没了），再断 stdin。
	if err := process.stdoutRead.Close(); err != nil {
		t.Fatalf("close stdout read end: %v", err)
	}
	process.closeStdin(t)

	code := process.waitExit(t, 30*time.Second)
	t.Logf("runtime exit code with broken stdout = %d; stderr=%q", code, process.stderr.String())
	waitE2EPIDExit(t, pythonPID)
	fixture.assertResourcesReleased(t)
}
