//go:build windows

package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
	"golang.org/x/sys/windows"
)

const (
	e2eFakeUVConfigEnv = "FAKE_UV_CONFIG"
	e2eFakeBackendEnv  = "FAKE_BACKEND_CONFIG"
	e2eRootEnv         = "BACKEND_E2E_ROOT"
	e2eSkipPortMutex   = "BACKEND_E2E_SKIP_PORT_MUTEX"
	e2eRuntimeHelper   = "BACKEND_E2E_RUNTIME_HELPER"
	e2eRuntimeSignal   = "BACKEND_E2E_RUNTIME_SIGNAL"
	e2ePortMutexName   = `Local\AUTO-MAS-RUNTIME-M6-E2E-PORT-36163`
	e2eOperationID     = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
)

type backendE2EConfig struct {
	ListenAddress            string             `json:"listenAddress,omitempty"`
	PIDFile                  string             `json:"pidFile,omitempty"`
	GrandchildPIDFile        string             `json:"grandchildPidFile,omitempty"`
	SpawnGrandchild          bool               `json:"spawnGrandchild,omitempty"`
	GrandchildLifetimeMS     int                `json:"grandchildLifetimeMs,omitempty"`
	LeaveGrandchildOnCrash   bool               `json:"leaveGrandchildOnCrash,omitempty"`
	Health                   []backendE2EHealth `json:"health,omitempty"`
	CloseStatus              int                `json:"closeStatus,omitempty"`
	CrashAfterHealthRequests int                `json:"crashAfterHealthRequests,omitempty"`
	CrashExitCode            int                `json:"crashExitCode,omitempty"`
	Events                   []backendE2EEvent  `json:"events,omitempty"`
}

type backendE2EHealth struct {
	Ready            bool    `json:"ready"`
	BackgroundStatus string  `json:"backgroundStatus"`
	BackgroundError  *string `json:"backgroundError"`
	Protocol         int     `json:"protocol"`
	Version          string  `json:"version,omitempty"`
	Commit           string  `json:"commit,omitempty"`
}

type backendE2EEvent struct {
	Stream    string `json:"stream"`
	Line      string `json:"line"`
	NoNewline bool   `json:"noNewline,omitempty"`
}

type backendE2ERuntimeSignal struct {
	Root                 string `json:"root"`
	AppRoot              string `json:"appRoot"`
	Repo                 string `json:"repo"`
	BackendState         string `json:"backendState"`
	PythonPID            string `json:"pythonPid"`
	GrandchildPID        string `json:"grandchildPid"`
	SupervisorPID        uint32 `json:"supervisorPid"`
	RootProcessPID       uint32 `json:"rootProcessPid"`
	PythonProcessPID     uint32 `json:"pythonProcessPid"`
	GrandchildProcessPID uint32 `json:"grandchildProcessPid"`
}

type backendE2EEmitter struct {
	mu       sync.Mutex
	states   []protocol.StateEvent
	logs     []protocol.LogEvent
	warnings []protocol.WarningEvent
	timeline []backendE2ETimelineEvent
	err      error
	wake     chan struct{}
}

type backendE2ETimelineEvent struct {
	kind    string
	state   protocol.StateEvent
	log     protocol.LogEvent
	warning protocol.WarningEvent
}

func newBackendE2EEmitter() *backendE2EEmitter {
	return &backendE2EEmitter{wake: make(chan struct{})}
}

func (e *backendE2EEmitter) EmitState(event protocol.StateEvent) error {
	e.mu.Lock()
	clone := cloneE2EState(event)
	e.states = append(e.states, clone)
	e.timeline = append(e.timeline, backendE2ETimelineEvent{kind: "state", state: clone})
	e.signalLocked()
	e.mu.Unlock()
	return nil
}

func (e *backendE2EEmitter) EmitLog(event protocol.LogEvent) error {
	e.mu.Lock()
	e.logs = append(e.logs, event)
	e.timeline = append(e.timeline, backendE2ETimelineEvent{kind: "log", log: event})
	e.signalLocked()
	e.mu.Unlock()
	return nil
}

func (e *backendE2EEmitter) EmitWarning(event protocol.WarningEvent) error {
	e.mu.Lock()
	e.warnings = append(e.warnings, event)
	e.timeline = append(e.timeline, backendE2ETimelineEvent{kind: "warning", warning: event})
	e.signalLocked()
	e.mu.Unlock()
	return nil
}

func (e *backendE2EEmitter) setError(err error) {
	e.mu.Lock()
	e.err = err
	e.signalLocked()
	e.mu.Unlock()
}

func (e *backendE2EEmitter) signalLocked() {
	close(e.wake)
	e.wake = make(chan struct{})
}

func (e *backendE2EEmitter) waitState(t *testing.T, status protocol.StateStatus, occurrence int) protocol.StateEvent {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		count := 0
		var found protocol.StateEvent
		for _, event := range e.states {
			if event.Status == status {
				count++
				if count == occurrence {
					found = cloneE2EState(event)
				}
			}
		}
		wake := e.wake
		err := e.err
		e.mu.Unlock()
		if count >= occurrence {
			return found
		}
		if err != nil {
			t.Fatalf("supervise returned before state %s occurrence %d: %s (states: %#v)", status, occurrence, describeE2EError(err), e.statesSnapshot())
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("timed out waiting for state %s occurrence %d (supervise error: %v, states: %#v)", status, occurrence, err, e.statesSnapshot())
		}
	}
}

func describeE2EError(err error) string {
	if err == nil {
		return "<nil>"
	}
	var uvErr *uv.Error
	if errors.As(err, &uvErr) {
		return fmt.Sprintf("%T code=%s stage=%s details=%#v cause=%T:%v", err, uvErr.Code(), uvErr.Stage(), uvErr.Details(), errors.Unwrap(uvErr), errors.Unwrap(uvErr))
	}
	return fmt.Sprintf("%T:%v", err, err)
}

func (e *backendE2EEmitter) statesSnapshot() []protocol.StateEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]protocol.StateEvent, len(e.states))
	for index, event := range e.states {
		result[index] = cloneE2EState(event)
	}
	return result
}

func (e *backendE2EEmitter) warningsSnapshot() []protocol.WarningEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.WarningEvent(nil), e.warnings...)
}

func (e *backendE2EEmitter) logsSnapshot() []protocol.LogEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.LogEvent(nil), e.logs...)
}

func (e *backendE2EEmitter) timelineSnapshot() []backendE2ETimelineEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]backendE2ETimelineEvent, len(e.timeline))
	for index, event := range e.timeline {
		result[index] = event
		if event.kind == "state" {
			result[index].state = cloneE2EState(event.state)
		}
	}
	return result
}

func cloneE2EState(event protocol.StateEvent) protocol.StateEvent {
	clone := event
	if event.Details != nil {
		clone.Details = make(map[string]any, len(event.Details))
		for key, value := range event.Details {
			clone.Details[key] = value
		}
	}
	return clone
}

type backendE2EFixture struct {
	t             *testing.T
	root          string
	appRoot       string
	repo          string
	configPath    string
	rootPID       string
	grandchildPID string
	uvExecReady   string
	uvExecRelease string
	layout        *config.Layout
	emitter       *backendE2EEmitter
	mailbox       *ControlMailbox
	supervisor    *ManagedSupervisor
	config        backendE2EConfig
	timerReady    chan *backendE2ETimer
}

// backendE2EPIDGeneration 保存一次启动中三个真实进程的同步句柄；句柄
// 在进程仍运行时打开，避免重启覆盖 PID 文件后误把新一代当成旧一代。
type backendE2EPIDGeneration struct {
	rootPID       uint32
	pythonPID     uint32
	grandchildPID uint32
	handles       []windows.Handle
}

func (f *backendE2EFixture) captureGeneration(t *testing.T, running protocol.StateEvent) backendE2EPIDGeneration {
	t.Helper()
	rootPID, ok := e2EUint32(running.Details["pid"])
	if !ok {
		t.Fatalf("running details pid = %#v, want uint32", running.Details["pid"])
	}
	rootFilePID := waitE2EPIDFile(t, f.rootPID)
	if rootFilePID != rootPID {
		t.Fatalf("uv root PID file = %d, running state pid = %d", rootFilePID, rootPID)
	}
	pythonPID := waitE2EPIDFile(t, f.config.PIDFile)
	grandchildPID := waitE2EPIDFile(t, f.grandchildPID)
	generation := backendE2EPIDGeneration{
		rootPID:       rootPID,
		pythonPID:     pythonPID,
		grandchildPID: grandchildPID,
	}
	for _, pid := range []uint32{rootPID, pythonPID, grandchildPID} {
		handle, err := openE2ESyncHandle(pid)
		if err != nil {
			if closeErr := generation.close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
			t.Fatalf("OpenProcess(%d) for active generation: %v", pid, err)
		}
		generation.handles = append(generation.handles, handle)
	}
	return generation
}

func (g *backendE2EPIDGeneration) wait(t *testing.T) {
	t.Helper()
	if g == nil {
		return
	}
	var waitErr error
	for index, handle := range g.handles {
		result, err := windows.WaitForSingleObject(handle, 10_000)
		if err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("WaitForSingleObject generation handle %d: %w", index, err))
		} else if result != windows.WAIT_OBJECT_0 {
			waitErr = errors.Join(waitErr, fmt.Errorf("generation handle %d wait result = %d", index, result))
		}
		if err := windows.CloseHandle(handle); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("CloseHandle generation handle %d: %w", index, err))
		}
	}
	g.handles = nil
	if waitErr != nil {
		t.Fatal(waitErr)
	}
}

func (g *backendE2EPIDGeneration) close() error {
	if g == nil {
		return nil
	}
	var closeErr error
	for _, handle := range g.handles {
		closeErr = errors.Join(closeErr, windows.CloseHandle(handle))
	}
	g.handles = nil
	return closeErr
}

func newBackendE2EFixture(t *testing.T, configValue backendE2EConfig) *backendE2EFixture {
	t.Helper()
	if os.Getenv(e2eSkipPortMutex) != "1" {
		acquireE2EPortMutex(t)
	}
	root := strings.TrimSpace(os.Getenv(e2eRootEnv))
	if root == "" {
		root = t.TempDir()
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll(E2E root) error = %v", err)
	}
	appRoot := filepath.Join(root, "runtime-root")
	if err := os.Mkdir(appRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(%q) error = %v", appRoot, err)
	}
	repo := filepath.Join(root, "development-repo")
	if err := os.MkdirAll(filepath.Join(repo, ".venv", "Scripts"), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", repo, err)
	}
	for name, payload := range map[string]string{
		"main.py":        "print('fake backend')\n",
		"pyproject.toml": "[project]\nname = 'fake-backend'\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(payload), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	layout, err := config.NewLayout(appRoot, filepath.Dir(appRoot))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	// 即使 development 前置检查也会使用生产 runner 的默认受管项目目录；
	// 仅保留这个诊断工作目录，不把开发文件写入 runtime-root。
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("MkdirAll(managed repo cwd) error = %v", err)
	}
	uvPath, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(uvPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(uv path) error = %v", err)
	}
	fakeUV := buildE2EFixture(t, filepath.Join("..", "..", "testdata", "fakeuv"), root, "fakeuv.exe")
	fakeBackend := buildE2EFixture(t, filepath.Join("..", "..", "testdata", "fakebackend"), root, "fakebackend.exe")
	if err := copyE2EFile(fakeUV, uvPath); err != nil {
		t.Fatalf("copy fake uv: %v", err)
	}
	pythonPath := layout.VenvPythonExecutable()
	if err := os.MkdirAll(filepath.Dir(pythonPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(python path) error = %v", err)
	}
	if err := copyE2EFile(fakeBackend, pythonPath); err != nil {
		t.Fatalf("copy fake backend: %v", err)
	}
	developmentPythonPath := filepath.Join(repo, ".venv", "Scripts", "python.exe")
	if err := copyE2EFile(fakeBackend, developmentPythonPath); err != nil {
		t.Fatalf("copy fake backend to development venv: %v", err)
	}
	configValue.ListenAddress = "127.0.0.1:36163"
	configValue.PIDFile = filepath.Join(root, "python.pid")
	configValue.GrandchildPIDFile = filepath.Join(root, "grandchild.pid")
	rootPIDPath := filepath.Join(root, "uv.pid")
	uvExecReadyPath := ""
	uvExecReleasePath := ""
	if configValue.LeaveGrandchildOnCrash {
		uvExecReadyPath = filepath.Join(root, "uv-exec-ready")
		uvExecReleasePath = filepath.Join(root, "uv-exec-release")
	}
	backendConfigPath := filepath.Join(root, "backend-config.json")
	writeE2EConfig(t, backendConfigPath, configValue)
	uvConfigPath := filepath.Join(root, "uv-config.json")
	writeE2EUVConfig(t, uvConfigPath, developmentPythonPath, rootPIDPath, uvExecReadyPath, uvExecReleasePath)
	t.Setenv(e2eFakeBackendEnv, backendConfigPath)
	t.Setenv(e2eFakeUVConfigEnv, uvConfigPath)
	if err := waitE2EPortClosed(t.Context()); err != nil {
		t.Fatalf("port 36163 is occupied before fixture: %v", err)
	}
	if err := assertE2EPortBindable(); err != nil {
		t.Fatalf("port 36163 cannot bind before fixture: %v", err)
	}
	supervisor, err := NewProductionManagedSupervisor(t.Context(), layout, io.Discard, time.Now)
	if err != nil {
		t.Fatalf("NewProductionManagedSupervisor() error = %v", err)
	}
	timerReady := make(chan *backendE2ETimer, 4)
	// E2E 只缩短注入的健康/重启预算，生产依赖仍使用真实实现。
	supervisor.deps.Health = health.NewChecker(health.Config{
		PollInterval:         25 * time.Millisecond,
		TotalTimeout:         15 * time.Second,
		RequestTimeout:       time.Second,
		ConsecutiveSuccesses: 2,
	})
	supervisor.deps.RestartDelay = 50 * time.Millisecond
	supervisor.deps.NewTimer = func(time.Duration) Timer {
		timer := newBackendE2ETimer()
		timerReady <- timer
		return timer
	}
	emitter := newBackendE2EEmitter()
	mailbox := NewControlMailbox(16)
	t.Cleanup(mailbox.Close)
	return &backendE2EFixture{
		t:             t,
		root:          root,
		appRoot:       appRoot,
		repo:          repo,
		configPath:    backendConfigPath,
		rootPID:       rootPIDPath,
		grandchildPID: configValue.GrandchildPIDFile,
		uvExecReady:   uvExecReadyPath,
		uvExecRelease: uvExecReleasePath,
		layout:        layout,
		emitter:       emitter,
		mailbox:       mailbox,
		supervisor:    supervisor,
		config:        configValue,
		timerReady:    timerReady,
	}
}

func (f *backendE2EFixture) request() Request {
	f.t.Helper()
	return Request{
		OperationID:        e2eOperationID,
		RuntimePID:         uint32(os.Getpid()),
		Mode:               ModeDevelopment,
		DevelopmentRepo:    f.repo,
		Emitter:            f.emitter,
		Control:            f.mailbox,
		BeforeShutdown:     f.mailbox.BeforeShutdown,
		BeforeControlClose: f.mailbox.StopAccepting,
	}
}

func (f *backendE2EFixture) supervise(ctx context.Context) <-chan error {
	f.t.Helper()
	if err := waitE2EPortClosed(ctx); err != nil {
		f.t.Fatalf("port 36163 is occupied before supervise: %v", err)
	}
	if err := assertE2EPortBindable(); err != nil {
		f.t.Fatalf("port 36163 cannot bind before supervise: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		err := f.supervisor.Supervise(ctx, f.request())
		f.emitter.setError(err)
		done <- err
	}()
	return done
}

func (f *backendE2EFixture) submitShutdown(t *testing.T) {
	t.Helper()
	if err := f.mailbox.Submit(t.Context(), protocol.ControlCommand{
		Command:   protocol.ControlShutdown,
		CommandID: e2eOperationID,
	}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
}

func (f *backendE2EFixture) waitRestartTimer(t *testing.T) *backendE2ETimer {
	t.Helper()
	select {
	case timer := <-f.timerReady:
		return timer
	case <-time.After(5 * time.Second):
		t.Fatal("restart timer was not created")
		return nil
	}
}

type backendE2ETimer struct {
	channel chan time.Time
	once    sync.Once
}

func newBackendE2ETimer() *backendE2ETimer {
	return &backendE2ETimer{channel: make(chan time.Time, 1)}
}

func (t *backendE2ETimer) C() <-chan time.Time { return t.channel }

func (t *backendE2ETimer) Stop() bool {
	return true
}

func (t *backendE2ETimer) Fire() {
	t.once.Do(func() { t.channel <- time.Now() })
}

func TestBackendE2E_LifecycleSpawnReadyShutdown(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:      true,
		GrandchildLifetimeMS: 60_000,
		Events:               e2EOutputEvents("lifecycle"),
	})
	done := fixture.supervise(t.Context())
	running := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	if running.Details["pid"] == nil || running.Details["logPath"] == nil {
		t.Fatalf("running details = %#v, want pid/logPath", running.Details)
	}
	generation := fixture.captureGeneration(t, running)
	assertE2ETransactionRunning(t, fixture)
	assertE2EBackendMutexHeld(t, fixture.layout)
	logPath, ok := running.Details["logPath"].(string)
	if !ok || logPath == "" {
		t.Fatalf("running logPath = %#v, want non-empty string", running.Details["logPath"])
	}
	if _, err := os.Stat(fixture.layout.BackendStateFile()); err != nil {
		t.Fatalf("backend transaction while running: %v", err)
	}
	fixture.submitShutdown(t)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	fixture.emitter.waitState(t, protocol.StateStopped, 1)
	fixture.assertResourcesReleased(t, &generation)
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateRunning, protocol.StateStoppingBackend, protocol.StateStopped)
	assertE2EPersistentLog(t, running, "lifecycle")
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:lifecycle ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:lifecycle ", "state:"+string(protocol.StateRunning))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateRunning), "state:"+string(protocol.StateStoppingBackend))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStoppingBackend), "state:"+string(protocol.StateStopped))
	logs := fixture.emitter.logsSnapshot()
	seen := map[string]bool{}
	for _, event := range logs {
		if strings.Contains(event.Message, "lifecycle stdout") && event.Stream == "stdout" {
			seen["stdout"] = true
		}
		if strings.Contains(event.Message, "lifecycle stderr") && event.Stream == "stderr" {
			seen["stderr"] = true
		}
	}
	if !seen["stdout"] || !seen["stderr"] {
		t.Fatalf("logs = %#v, want flushed stdout/stderr records", logs)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read runtime log %q: %v", logPath, err)
	}
	if !strings.Contains(string(payload), "lifecycle stdout") || !strings.Contains(string(payload), "lifecycle stderr") {
		t.Fatalf("runtime log = %q, want both stream messages", string(payload))
	}
}

func TestBackendE2E_PreReadyExit(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		Health:                   []backendE2EHealth{{Ready: false, BackgroundStatus: "starting", Protocol: 1}},
		CrashAfterHealthRequests: 100,
		CrashExitCode:            73,
		Events:                   e2EOutputEvents("preready"),
	})
	done := fixture.supervise(t.Context())
	fixture.emitter.waitState(t, protocol.StateStartingBackend, 1)
	rootPID := waitE2EPIDFile(t, fixture.rootPID)
	pythonPID := waitE2EPIDFile(t, fixture.config.PIDFile)
	activeGeneration := backendE2EPIDGeneration{rootPID: rootPID, pythonPID: pythonPID}
	t.Cleanup(func() {
		if err := activeGeneration.close(); err != nil {
			t.Errorf("close pre-ready process handles: %v", err)
		}
	})
	for _, pid := range []uint32{rootPID, pythonPID} {
		handle, err := openE2ESyncHandle(pid)
		if err != nil {
			if closeErr := activeGeneration.close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
			t.Fatalf("OpenProcess(%d) while pre-ready process is active: %v", pid, err)
		}
		if result, waitErr := windows.WaitForSingleObject(handle, 0); waitErr != nil || result != uint32(windows.WAIT_TIMEOUT) {
			closeErr := windows.CloseHandle(handle)
			t.Fatalf("pre-ready process %d is not active: wait=%d error=%v close=%v", pid, result, waitErr, closeErr)
		}
		activeGeneration.handles = append(activeGeneration.handles, handle)
	}
	err := <-done
	assertBackendCode(t, err, protocol.CodeBackendExitedBeforeReady)
	activeGeneration.wait(t)
	fixture.emitter.waitState(t, protocol.StateBackendFailed, 1)
	for _, event := range fixture.emitter.statesSnapshot() {
		if event.Status == protocol.StateRunning || event.Status == protocol.StateRestarting {
			t.Fatalf("pre-ready states = %#v, must not contain running/restarting", fixture.emitter.statesSnapshot())
		}
	}
	if payload, readErr := os.ReadFile(fixture.config.PIDFile); readErr == nil {
		pid, parseErr := parseE2EPID(payload)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		waitE2EPIDExit(t, pid)
	}
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateBackendFailed)
	assertE2EFailureFacts(t, fixture.emitter.statesSnapshot())
	failed := fixture.emitter.statesSnapshot()[len(fixture.emitter.statesSnapshot())-1]
	if exitCode, ok := e2EExitCode(failed.Details["exitCode"]); !ok || exitCode != 73 {
		t.Fatalf("pre-ready exitCode = %#v, want 73", failed.Details["exitCode"])
	}
	assertE2EPersistentLog(t, failed, "preready")
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:preready ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:preready ", "state:"+string(protocol.StateBackendFailed))
	fixture.assertResourcesReleased(t)
}

func TestBackendE2E_FirstCrashRestartSuccess(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:          true,
		GrandchildLifetimeMS:     60_000,
		CrashAfterHealthRequests: 5,
		CrashExitCode:            73,
		Events:                   e2EOutputEvents("first"),
	})
	done := fixture.supervise(t.Context())
	running1 := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	generation1 := fixture.captureGeneration(t, running1)
	assertE2ETransactionRunning(t, fixture)
	assertE2EBackendMutexHeld(t, fixture.layout)
	triggerE2ECrash(t)
	fixture.emitter.waitState(t, protocol.StateRestarting, 1)
	fixture.config.CrashAfterHealthRequests = 0
	fixture.config.Events = e2EOutputEvents("first-restart")
	writeE2EConfig(t, fixture.configPath, fixture.config)
	fixture.waitRestartTimer(t).Fire()
	running2 := fixture.emitter.waitState(t, protocol.StateRunning, 2)
	generation2 := fixture.captureGeneration(t, running2)
	fixture.submitShutdown(t)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	fixture.assertResourcesReleased(t, &generation1, &generation2)
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateRunning, protocol.StateRestarting, protocol.StateRunning, protocol.StateStoppingBackend, protocol.StateStopped)
	log1 := assertE2EPersistentLog(t, running1, "first")
	log2 := assertE2EPersistentLog(t, running2, "first-restart")
	if log1 != log2 {
		t.Fatalf("restart log paths = %q and %q, want the operation's persistent log path", log1, log2)
	}
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:first ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:first ", "state:"+string(protocol.StateRestarting))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateRestarting), "log:first-restart ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:first-restart ", "state:"+string(protocol.StateStoppingBackend))
	for _, event := range []protocol.StateEvent{running1, running2} {
		if event.Details["pid"] == nil || event.Details["logPath"] == nil {
			t.Fatalf("running event details = %#v, want pid/logPath facts", event.Details)
		}
	}
}

func TestBackendE2E_SecondCrashTerminates(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:          true,
		GrandchildLifetimeMS:     60_000,
		CrashAfterHealthRequests: 5,
		CrashExitCode:            73,
		Events:                   e2EOutputEvents("second"),
	})
	done := fixture.supervise(t.Context())
	running1 := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	generation1 := fixture.captureGeneration(t, running1)
	triggerE2ECrash(t)
	fixture.emitter.waitState(t, protocol.StateRestarting, 1)
	fixture.config.Events = e2EOutputEvents("second-restart")
	writeE2EConfig(t, fixture.configPath, fixture.config)
	fixture.waitRestartTimer(t).Fire()
	running2 := fixture.emitter.waitState(t, protocol.StateRunning, 2)
	generation2 := fixture.captureGeneration(t, running2)
	triggerE2ECrash(t)
	err := <-done
	assertBackendCode(t, err, protocol.CodeBackendExitedUnexpectedly)
	if got := countE2EStates(fixture.emitter.statesSnapshot(), protocol.StateRunning); got != 2 {
		t.Fatalf("running state count = %d, want 2", got)
	}
	fixture.emitter.waitState(t, protocol.StateBackendFailed, 1)
	fixture.assertResourcesReleased(t, &generation1, &generation2)
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateRunning, protocol.StateRestarting, protocol.StateRunning, protocol.StateBackendFailed)
	assertE2EFailureFacts(t, fixture.emitter.statesSnapshot())
	assertE2EPersistentLog(t, running1, "second")
	failed := fixture.emitter.statesSnapshot()[len(fixture.emitter.statesSnapshot())-1]
	assertE2EPersistentLog(t, running2, "second-restart")
	if exitCode, ok := e2EExitCode(failed.Details["exitCode"]); !ok || exitCode != 73 {
		t.Fatalf("backend_failed exitCode = %#v, want 73", failed.Details["exitCode"])
	}
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:second ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:second ", "state:"+string(protocol.StateRestarting))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateRestarting), "log:second-restart ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:second-restart ", "state:"+string(protocol.StateBackendFailed))
}

func TestBackendE2E_ForcedShutdownReapsTree(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:      true,
		GrandchildLifetimeMS: 60_000,
		CloseStatus:          503,
		Events:               e2EOutputEvents("forced"),
	})
	done := fixture.supervise(t.Context())
	running := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	generation := fixture.captureGeneration(t, running)
	assertE2ETransactionRunning(t, fixture)
	assertE2EBackendMutexHeld(t, fixture.layout)
	fixture.submitShutdown(t)
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	fixture.emitter.waitState(t, protocol.StateStopped, 1)
	warnings := fixture.emitter.warningsSnapshot()
	if len(warnings) == 0 || warnings[len(warnings)-1].Code != string(protocol.CodeBackendForceTerminated) {
		t.Fatalf("warnings = %#v, want BACKEND_FORCE_TERMINATED", warnings)
	}
	fixture.assertResourcesReleased(t, &generation)
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateRunning, protocol.StateStoppingBackend, protocol.StateStopped)
	assertE2EPersistentLog(t, running, "forced")
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:forced ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:forced ", "state:"+string(protocol.StateStoppingBackend))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStoppingBackend), "warning:"+string(protocol.CodeBackendForceTerminated))
	assertE2ETimelineBefore(t, fixture.emitter, "warning:"+string(protocol.CodeBackendForceTerminated), "state:"+string(protocol.StateStopped))
}

func TestBackendE2E_RuntimeTerminationLeavesNoDescendants(t *testing.T) {
	if os.Getenv(e2eRuntimeHelper) == "1" {
		runBackendE2ERuntimeHelper(t)
		return
	}
	acquireE2EPortMutex(t)
	root := t.TempDir()
	signalPath := filepath.Join(root, "runtime-helper.signal")
	outputPath := filepath.Join(root, "runtime-helper.log")
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open runtime helper log: %v", err)
	}
	helper := exec.Command(os.Args[0], "-test.run", "^TestBackendE2E_RuntimeTerminationLeavesNoDescendants$", "-test.v")
	helper.Env = append(os.Environ(),
		e2eRuntimeHelper+"=1",
		e2eRootEnv+"="+root,
		e2eRuntimeSignal+"="+signalPath,
		e2eSkipPortMutex+"=1",
	)
	helper.Stdout = output
	helper.Stderr = output
	if err := helper.Start(); err != nil {
		t.Fatalf("start runtime helper: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed && helper.Process != nil {
			if err := terminateE2EProcess(helper.Process.Pid); err != nil {
				t.Errorf("cleanup TerminateProcess(runtime helper): %v", err)
			}
			if err := helper.Wait(); err != nil {
				t.Errorf("cleanup wait runtime helper: %v", err)
			}
		}
		if err := output.Close(); err != nil {
			t.Errorf("close runtime helper log: %v", err)
		}
	})
	signal := waitBackendE2ERuntimeSignal(t, signalPath)
	if signal.RootProcessPID == 0 || signal.SupervisorPID == 0 || signal.PythonProcessPID == 0 || signal.GrandchildProcessPID == 0 {
		t.Fatalf("runtime signal = %#v, want helper/uv/Python/grandchild PIDs", signal)
	}
	var runtimeHandles []windows.Handle
	for _, pid := range []uint32{signal.SupervisorPID, signal.RootProcessPID, signal.PythonProcessPID, signal.GrandchildProcessPID} {
		handle, openErr := openE2ESyncHandle(pid)
		if openErr != nil {
			if closeErr := closeE2EHandles(runtimeHandles); closeErr != nil {
				openErr = errors.Join(openErr, closeErr)
			}
			t.Fatalf("OpenProcess(%d) before runtime termination: %v", pid, openErr)
		}
		runtimeHandles = append(runtimeHandles, handle)
	}
	if err := terminateE2EProcess(helper.Process.Pid); err != nil {
		if closeErr := closeE2EHandles(runtimeHandles); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		t.Fatalf("TerminateProcess(runtime helper) error = %v", err)
	}
	killed = true
	if err := helper.Wait(); err == nil {
		t.Fatal("runtime helper exited successfully, want TerminateProcess interruption")
	}
	waitE2EHandles(t, runtimeHandles)
	if err := waitE2EPortClosed(t.Context()); err != nil {
		t.Fatalf("port after runtime termination: %v", err)
	}
	if payload, err := os.ReadFile(signal.BackendState); err != nil {
		t.Fatalf("read stale backend transaction: %v", err)
	} else {
		var transaction state.TransactionState
		if err := json.Unmarshal(payload, &transaction); err != nil {
			t.Fatalf("stale backend transaction = %s, decode error = %v", payload, err)
		}
		if transaction.SchemaVersion != state.SchemaVersion || transaction.OperationID != e2eOperationID ||
			transaction.Command != "backend supervise" || transaction.PID != signal.SupervisorPID ||
			transaction.Stage != protocol.StageBackendRun || transaction.TargetVersion != "" {
			t.Fatalf("stale backend transaction = %#v, want valid backend.run facts", transaction)
		}
	}
	layout := mustE2ELayout(t, signal.AppRoot)
	if err := assertE2EBackendMutexReleased(t, layout); err != nil {
		t.Fatalf("backend mutex after runtime termination: %v", err)
	}

	// 新的真实监督器必须在下一次 development 尝试前恢复陈旧事务，
	// 并在正常关闭时删除它。
	t.Setenv(e2eFakeBackendEnv, filepath.Join(signal.Root, "backend-config.json"))
	t.Setenv(e2eFakeUVConfigEnv, filepath.Join(signal.Root, "uv-config.json"))
	supervisor, err := NewProductionManagedSupervisor(t.Context(), layout, io.Discard, time.Now)
	if err != nil {
		t.Fatalf("NewProductionManagedSupervisor(recovery) error = %v", err)
	}
	supervisor.deps.Health = health.NewChecker(health.Config{
		PollInterval:         25 * time.Millisecond,
		TotalTimeout:         15 * time.Second,
		RequestTimeout:       time.Second,
		ConsecutiveSuccesses: 2,
	})
	emitter := newBackendE2EEmitter()
	mailbox := NewControlMailbox(16)
	t.Cleanup(mailbox.Close)
	done := make(chan error, 1)
	go func() {
		done <- supervisor.Supervise(t.Context(), Request{
			OperationID:        e2eOperationID,
			RuntimePID:         uint32(os.Getpid()),
			Mode:               ModeDevelopment,
			DevelopmentRepo:    signal.Repo,
			Emitter:            emitter,
			Control:            mailbox,
			BeforeShutdown:     mailbox.BeforeShutdown,
			BeforeControlClose: mailbox.StopAccepting,
		})
	}()
	emitter.waitState(t, protocol.StateRunning, 1)
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: e2eOperationID}); err != nil {
		t.Fatalf("recovery shutdown submit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("recovery supervisor error = %v", err)
	}
	recovery := &backendE2EFixture{layout: layout, grandchildPID: signal.GrandchildPID}
	recovery.assertResourcesReleased(t)
}

func runBackendE2ERuntimeHelper(t *testing.T) {
	t.Helper()
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:      true,
		GrandchildLifetimeMS: 60_000,
	})
	fixture.supervise(t.Context())
	running := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	rootPID, ok := e2EUint32(running.Details["pid"])
	if !ok {
		t.Fatalf("running details pid = %#v, want uint32", running.Details["pid"])
	}
	pythonPID := waitE2EPIDFile(t, fixture.config.PIDFile)
	grandchildPID := waitE2EPIDFile(t, fixture.grandchildPID)
	signalPath := strings.TrimSpace(os.Getenv(e2eRuntimeSignal))
	if signalPath == "" {
		t.Fatal("runtime helper signal path is empty")
	}
	payload, err := json.Marshal(backendE2ERuntimeSignal{
		Root:                 fixture.root,
		AppRoot:              fixture.appRoot,
		Repo:                 fixture.repo,
		BackendState:         fixture.layout.BackendStateFile(),
		PythonPID:            fixture.config.PIDFile,
		GrandchildPID:        fixture.grandchildPID,
		SupervisorPID:        uint32(os.Getpid()),
		RootProcessPID:       rootPID,
		PythonProcessPID:     pythonPID,
		GrandchildProcessPID: grandchildPID,
	})
	if err != nil {
		t.Fatalf("marshal runtime signal: %v", err)
	}
	if err := writeE2ERuntimeSignal(signalPath, payload); err != nil {
		t.Fatalf("write runtime signal: %v", err)
	}
	select {}
}

func TestBackendE2E_DescendantHoldingPipeCannotBlockCleanup(t *testing.T) {
	fixture := newBackendE2EFixture(t, backendE2EConfig{
		SpawnGrandchild:          true,
		GrandchildLifetimeMS:     60_000,
		LeaveGrandchildOnCrash:   true,
		CrashAfterHealthRequests: 5,
		CrashExitCode:            73,
		Events:                   e2EOutputEvents("holding"),
	})
	done := fixture.supervise(t.Context())
	running1 := fixture.emitter.waitState(t, protocol.StateRunning, 1)
	generation1 := fixture.captureGeneration(t, running1)
	assertE2ETransactionRunning(t, fixture)
	crashStarted := time.Now()
	triggerE2ECrash(t)
	waitE2EFile(t, fixture.uvExecReady)
	if result, err := windows.WaitForSingleObject(generation1.handles[0], 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("uv root during descendant cleanup = result %d, err %v, want WAIT_TIMEOUT", result, err)
	}
	if result, err := windows.WaitForSingleObject(generation1.handles[1], 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("Python during descendant cleanup = result %d, err %v, want WAIT_OBJECT_0", result, err)
	}
	if result, err := windows.WaitForSingleObject(generation1.handles[2], 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("grandchild during descendant cleanup = result %d, err %v, want WAIT_TIMEOUT", result, err)
	}
	if err := writeE2ERuntimeSignal(fixture.uvExecRelease, []byte("release\n")); err != nil {
		t.Fatalf("release uv exit barrier: %v", err)
	}
	fixture.emitter.waitState(t, protocol.StateRestarting, 1)
	if elapsed := time.Since(crashStarted); elapsed >= 10*time.Second {
		t.Fatalf("crash to restarting elapsed %s, want <10s", elapsed)
	}
	generation1.wait(t)
	fixture.config.CrashAfterHealthRequests = 0
	fixture.config.LeaveGrandchildOnCrash = false
	fixture.config.Events = e2EOutputEvents("holding-restart")
	writeE2EConfig(t, fixture.configPath, fixture.config)
	fixture.waitRestartTimer(t).Fire()
	running2 := fixture.emitter.waitState(t, protocol.StateRunning, 2)
	generation2 := fixture.captureGeneration(t, running2)
	fixture.submitShutdown(t)
	start := time.Now()
	err := <-done
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cleanup elapsed %s, want bounded descendant pipe cleanup", elapsed)
	}
	if err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	fixture.assertResourcesReleased(t, &generation2)
	assertE2EStateSequence(t, fixture.emitter.statesSnapshot(), protocol.StateStartingBackend, protocol.StateRunning, protocol.StateRestarting, protocol.StateRunning, protocol.StateStoppingBackend, protocol.StateStopped)
	log1 := assertE2EPersistentLog(t, running1, "holding")
	log2 := assertE2EPersistentLog(t, running2, "holding-restart")
	if log1 != log2 {
		t.Fatalf("holding restart log paths = %q and %q, want the operation's persistent log path", log1, log2)
	}
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateStartingBackend), "log:holding ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:holding ", "state:"+string(protocol.StateRunning))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateRunning), "state:"+string(protocol.StateRestarting))
	assertE2ETimelineBefore(t, fixture.emitter, "state:"+string(protocol.StateRestarting), "log:holding-restart ")
	assertE2ETimelineBefore(t, fixture.emitter, "log:holding-restart ", "state:"+string(protocol.StateStoppingBackend))
}

func countE2EStates(states []protocol.StateEvent, want protocol.StateStatus) int {
	count := 0
	for _, event := range states {
		if event.Status == want {
			count++
		}
	}
	return count
}

func assertE2EStateSequence(t *testing.T, events []protocol.StateEvent, want ...protocol.StateStatus) {
	t.Helper()
	got := make([]protocol.StateStatus, 0, len(events))
	for _, event := range events {
		if event.Status == protocol.StateReadyToStart {
			continue
		}
		got = append(got, event.Status)
	}
	if len(got) != len(want) {
		t.Fatalf("state sequence = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("state sequence[%d] = %s, want %s (all %#v)", index, got[index], want[index], got)
		}
	}
}

func assertE2ETimelineBefore(t *testing.T, emitter *backendE2EEmitter, before, after string) {
	t.Helper()
	items := emitter.timelineSnapshot()
	beforeIndex := timelineIndex(items, before)
	afterIndex := timelineIndex(items, after)
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("timeline order %q before %q not established: %#v", before, after, describeE2ETimeline(items))
	}
}

func timelineIndex(items []backendE2ETimelineEvent, prefix string) int {
	for index, item := range items {
		key := timelineKey(item)
		if strings.HasPrefix(key, prefix) {
			return index
		}
	}
	return -1
}

func timelineKey(item backendE2ETimelineEvent) string {
	switch item.kind {
	case "state":
		return "state:" + string(item.state.Status)
	case "log":
		return "log:" + item.log.Message
	case "warning":
		return "warning:" + item.warning.Code
	default:
		return item.kind
	}
}

func describeE2ETimeline(items []backendE2ETimelineEvent) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, timelineKey(item))
	}
	return result
}

func assertE2ETransactionRunning(t *testing.T, fixture *backendE2EFixture) {
	t.Helper()
	payload, err := os.ReadFile(fixture.layout.BackendStateFile())
	if err != nil {
		t.Fatalf("read backend transaction: %v", err)
	}
	var transaction state.TransactionState
	if err := json.Unmarshal(payload, &transaction); err != nil {
		t.Fatalf("decode backend transaction: %v", err)
	}
	wantPID := uint32(os.Getpid())
	if transaction.SchemaVersion != state.SchemaVersion ||
		transaction.OperationID != e2eOperationID ||
		transaction.Command != "backend supervise" ||
		transaction.PID != wantPID ||
		transaction.Stage != protocol.StageBackendRun ||
		transaction.TargetVersion != "" {
		t.Fatalf("backend transaction = %#v, want operation/command/pid/stage/backend.run/empty target", transaction)
	}
}

func assertE2EFailureFacts(t *testing.T, events []protocol.StateEvent) {
	t.Helper()
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Status != protocol.StateBackendFailed {
			continue
		}
		details := events[index].Details
		if details["pid"] == nil || details["logPath"] == nil || details["exitCode"] == nil {
			t.Fatalf("backend_failed details = %#v, want pid/logPath/exitCode facts", details)
		}
		return
	}
	t.Fatalf("states = %#v, want backend_failed facts", events)
}

func e2EOutputEvents(prefix string) []backendE2EEvent {
	return []backendE2EEvent{
		{Stream: "stdout", Line: prefix + " stdout"},
		{Stream: "stdout", Line: prefix + " stdout-tail", NoNewline: true},
		{Stream: "stdout", Line: prefix + " stdout-flush"},
		{Stream: "stderr", Line: prefix + " stderr"},
		{Stream: "stderr", Line: prefix + " stderr-tail", NoNewline: true},
		{Stream: "stderr", Line: prefix + " stderr-flush"},
	}
}

func assertE2EPersistentLog(t *testing.T, event protocol.StateEvent, prefix string) string {
	t.Helper()
	path, ok := event.Details["logPath"].(string)
	if !ok || path == "" {
		t.Fatalf("state %s details = %#v, want logPath", event.Status, event.Details)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime log %q: %v", path, err)
	}
	for _, suffix := range []string{" stdout", " stdout-tail", " stdout-flush", " stderr", " stderr-tail", " stderr-flush"} {
		if !strings.Contains(string(payload), prefix+suffix) {
			t.Fatalf("runtime log %q = %q, missing %q", path, string(payload), prefix+suffix)
		}
	}
	return path
}

func (f *backendE2EFixture) assertResourcesReleased(t *testing.T, generations ...*backendE2EPIDGeneration) {
	t.Helper()
	for _, generation := range generations {
		generation.wait(t)
	}
	if _, err := os.Stat(f.layout.BackendStateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backend transaction path = %v, want absent", err)
	}
	if entries, err := os.ReadDir(f.layout.StateDir()); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".backend.temp.") || strings.HasPrefix(name, ".backend.backup.") || name == ".backend.intent" {
				t.Fatalf("backend state residue %q", name)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadDir(%q) error = %v", f.layout.StateDir(), err)
	}
	if err := assertE2EPortClosed(); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(f.grandchildPID); err == nil {
		pid, parseErr := parseE2EPID(payload)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		waitE2EPIDExit(t, pid)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile(grandchild PID) error = %v", err)
	}
	if err := assertE2EBackendMutexReleased(t, f.layout); err != nil {
		t.Fatal(err)
	}
}

func acquireE2EPortMutex(t *testing.T) {
	t.Helper()
	ready := make(chan windows.Handle, 1)
	setupErr := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	ownerErr := make(chan error, 2)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		name, err := windows.UTF16PtrFromString(e2ePortMutexName)
		if err != nil {
			setupErr <- fmt.Errorf("UTF16PtrFromString: %w", err)
			close(done)
			return
		}
		handle, err := windows.CreateMutex(nil, false, name)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			setupErr <- fmt.Errorf("CreateMutex: %w", err)
			close(done)
			return
		}
		result, waitErr := windows.WaitForSingleObject(handle, 60_000)
		if waitErr != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
			closeErr := windows.CloseHandle(handle)
			if waitErr == nil {
				waitErr = fmt.Errorf("unexpected wait result")
			}
			setupErr <- errors.Join(fmt.Errorf("WaitForSingleObject = %d: %w", result, waitErr), closeErr)
			close(done)
			return
		}
		ready <- handle
		<-release
		if err := windows.ReleaseMutex(handle); err != nil {
			ownerErr <- fmt.Errorf("ReleaseMutex: %w", err)
		}
		if err := windows.CloseHandle(handle); err != nil {
			ownerErr <- fmt.Errorf("CloseHandle: %w", err)
		}
		close(done)
	}()
	select {
	case <-ready:
	case err := <-setupErr:
		t.Fatalf("acquire E2E port mutex: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for E2E port mutex owner")
		}
		close(ownerErr)
		for err := range ownerErr {
			t.Errorf("E2E port mutex cleanup: %v", err)
		}
	})
}

func assertE2EBackendMutexReleased(t *testing.T, layout *config.Layout) error {
	t.Helper()
	set, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		return fmt.Errorf("reopen backend mutex set: %w", err)
	}
	result, err := set.AcquireBackend(context.Background())
	if err != nil {
		if closeErr := set.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("reacquire backend mutex: %w", err), fmt.Errorf("close backend mutex set: %w", closeErr))
		}
		return fmt.Errorf("reacquire backend mutex: %w", err)
	}
	var resultErr error
	if lease := result.Lease(); lease != nil {
		if err := lease.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release reacquired backend mutex: %w", err))
		}
	}
	if err := set.Close(); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("close backend mutex set: %w", err))
	}
	return resultErr
}

func assertE2EBackendMutexHeld(t *testing.T, layout *config.Layout) {
	t.Helper()
	set, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		t.Fatalf("open backend mutex set while running: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	result, acquireErr := set.AcquireBackend(ctx)
	cancel()
	if lease := result.Lease(); lease != nil {
		if err := lease.Close(); err != nil {
			acquireErr = errors.Join(acquireErr, fmt.Errorf("release unexpected backend lease: %w", err))
		}
	}
	if closeErr := set.Close(); closeErr != nil {
		acquireErr = errors.Join(acquireErr, fmt.Errorf("close backend mutex set: %w", closeErr))
	}
	if acquireErr == nil {
		t.Fatal("AcquireBackend while supervised succeeded, want BACKEND_ALREADY_RUNNING")
	}
	var coded interface{ Code() protocol.Code }
	if !errors.As(acquireErr, &coded) || coded.Code() != protocol.CodeBackendAlreadyRunning {
		t.Fatalf("AcquireBackend while supervised error = %v, want %s", acquireErr, protocol.CodeBackendAlreadyRunning)
	}
}

func assertE2EPortClosed() error {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:36163", 200*time.Millisecond)
	if err == nil {
		return errors.Join(errors.New("backend port 36163 still accepts connections"), conn.Close())
	}
	return nil
}

func assertE2EPortBindable() error {
	listener, err := net.Listen("tcp4", "127.0.0.1:36163")
	if err != nil {
		return fmt.Errorf("bind/listen 127.0.0.1:36163: %w", err)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("close bind/listen probe: %w", err)
	}
	return nil
}

func waitE2EPortClosed(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := assertE2EPortClosed(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("backend port 36163 remained occupied")
		case <-ticker.C:
		}
	}
}

func mustE2ELayout(t *testing.T, appRoot string) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(appRoot, filepath.Dir(appRoot))
	if err != nil {
		t.Fatalf("NewLayout(%q) error = %v", appRoot, err)
	}
	return layout
}

func waitBackendE2ERuntimeSignal(t *testing.T, path string) backendE2ERuntimeSignal {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		payload, err := os.ReadFile(path)
		if err == nil {
			var signal backendE2ERuntimeSignal
			decodeErr := json.Unmarshal(payload, &signal)
			if decodeErr == nil {
				return signal
			}
			lastErr = fmt.Errorf("decode signal: %w", decodeErr)
		} else if !errors.Is(err, os.ErrNotExist) {
			lastErr = fmt.Errorf("read signal: %w", err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			log, logErr := os.ReadFile(filepath.Join(filepath.Dir(path), "runtime-helper.log"))
			t.Fatalf("timed out waiting for runtime helper signal: %v; last=%v; helperLog=%q (read=%v)", path, lastErr, log, logErr)
		}
	}
}

func writeE2ERuntimeSignal(path string, payload []byte) (resultErr error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".runtime-helper-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			resultErr = errors.Join(resultErr, os.Remove(temporaryPath))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(payload); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func waitE2EPIDFile(t *testing.T, path string) uint32 {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		payload, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := parseE2EPID(payload)
			if parseErr == nil {
				return pid
			}
			lastErr = parseErr
		} else if !errors.Is(err, os.ErrNotExist) {
			lastErr = err
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for PID file %q: %v", path, lastErr)
		}
	}
}

func waitE2EFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			lastErr = err
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for file %q: %v", path, lastErr)
		}
	}
}

func terminateE2EProcess(pid int) error {
	if pid <= 0 {
		return errors.New("runtime helper PID is invalid")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	var resultErr error
	if err := windows.TerminateProcess(handle, 137); err != nil {
		resultErr = errors.Join(resultErr, err)
	} else {
		result, err := windows.WaitForSingleObject(handle, 10_000)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		} else if result != windows.WAIT_OBJECT_0 {
			resultErr = errors.Join(resultErr, fmt.Errorf("TerminateProcess wait result = %d", result))
		}
	}
	if err := windows.CloseHandle(handle); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	if resultErr != nil {
		return resultErr
	}
	return nil
}

func e2EUint32(value any) (uint32, bool) {
	switch number := value.(type) {
	case uint32:
		return number, number != 0
	case int:
		return uint32(number), number > 0
	case int64:
		return uint32(number), number > 0 && number <= int64(^uint32(0))
	case float64:
		return uint32(number), number > 0 && number <= float64(^uint32(0)) && number == float64(uint32(number))
	default:
		return 0, false
	}
}

func e2EExitCode(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int32:
		return int(number), true
	case int64:
		return int(number), int64(int(number)) == number
	case float64:
		return int(number), number == float64(int(number))
	default:
		return 0, false
	}
}

func triggerE2ECrash(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for request := 0; request < 20; request++ {
		if err := triggerE2EHealthRequest(ctx); err != nil {
			// 连接被重置/拒绝表示假后端已越过崩溃阈值；随后状态断言
			// 负责证明生命周期确实完成了退出。
			return
		}
	}
	t.Fatalf("fake backend did not exit after 20 health requests")
}

func triggerE2EHealthRequest(ctx context.Context) error {
	client := &net.Dialer{Timeout: time.Second}
	conn, err := client.DialContext(ctx, "tcp", "127.0.0.1:36163")
	if err != nil {
		return err
	}
	request := "GET /api/core/health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return errors.Join(err, conn.Close())
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return errors.Join(err, conn.Close())
	}
	if err := conn.Close(); err != nil {
		return err
	}
	return nil
}

func buildE2EFixture(t *testing.T, relativeSource, root, name string) string {
	t.Helper()
	sourceDir, err := filepath.Abs(relativeSource)
	if err != nil {
		t.Fatalf("resolve fixture source: %v", err)
	}
	executable := filepath.Join(root, name)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", executable, ".")
	command.Dir = sourceDir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture %q: %v\n%s", sourceDir, err, output)
	}
	return executable
}

func copyE2EFile(source, destination string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, payload, 0o700)
}

func writeE2EConfig(t *testing.T, path string, configValue backendE2EConfig) {
	t.Helper()
	payload, err := json.Marshal(configValue)
	if err != nil {
		t.Fatalf("Marshal(fake backend config) error = %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func writeE2EUVConfig(t *testing.T, path, fakeBackend, pidFile, execReadyFile, execReleaseFile string) {
	t.Helper()
	runAction := map[string]any{
		"argumentsPrefix": []string{"run"},
		"exec":            fakeBackend,
		"pidFile":         pidFile,
	}
	if execReadyFile != "" {
		runAction["execReadyFile"] = execReadyFile
		runAction["execReleaseFile"] = execReleaseFile
	}
	configValue := map[string]any{
		"rules": []map[string]any{
			{"argumentsPrefix": []string{"--version"}, "stdout": []string{"uv 0.12.3"}},
			runAction,
		},
	}
	payload, err := json.Marshal(configValue)
	if err != nil {
		t.Fatalf("Marshal(fake uv config) error = %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func parseE2EPID(payload []byte) (uint32, error) {
	var value uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(payload)), "%d", &value); err != nil || value == 0 || value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("invalid PID file %q", string(payload))
	}
	return uint32(value), nil
}

func waitE2EPIDExit(t *testing.T, pid uint32) {
	t.Helper()
	handle, err := openE2ESyncHandle(pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("OpenProcess(%d) error = %v", pid, err)
	}
	result, err := windows.WaitForSingleObject(handle, 10_000)
	closeErr := windows.CloseHandle(handle)
	if err != nil || result != windows.WAIT_OBJECT_0 || closeErr != nil {
		t.Fatalf("WaitForSingleObject(%d) = %d, wait=%v close=%v", pid, result, err, closeErr)
	}
}

func openE2ESyncHandle(pid uint32) (windows.Handle, error) {
	if pid == 0 {
		return 0, errors.New("process PID is zero")
	}
	return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
}

func waitE2EHandles(t *testing.T, handles []windows.Handle) {
	t.Helper()
	var waitErr error
	for index, handle := range handles {
		result, err := windows.WaitForSingleObject(handle, 10_000)
		if err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("WaitForSingleObject handle %d: %w", index, err))
		} else if result != windows.WAIT_OBJECT_0 {
			waitErr = errors.Join(waitErr, fmt.Errorf("handle %d wait result = %d", index, result))
		}
		if err := windows.CloseHandle(handle); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("CloseHandle handle %d: %w", index, err))
		}
	}
	if waitErr != nil {
		t.Fatal(waitErr)
	}
}

func closeE2EHandles(handles []windows.Handle) error {
	var closeErr error
	for _, handle := range handles {
		closeErr = errors.Join(closeErr, windows.CloseHandle(handle))
	}
	return closeErr
}
