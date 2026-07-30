//go:build windows

package lock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const helperDeadline = 10 * time.Second

type aliasCommandResult struct {
	command    string
	output     []byte
	err        error
	contextErr error
}

func aliasCommandContext(parent context.Context) (
	context.Context,
	context.CancelFunc,
) {
	return context.WithTimeout(parent, helperDeadline)
}

func runAliasCommand(
	parent context.Context,
	environment []string,
	name string,
	args ...string,
) aliasCommandResult {
	ctx, cancel := aliasCommandContext(parent)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	if environment != nil {
		command.Env = environment
	}
	output, err := command.CombinedOutput()
	return aliasCommandResult{
		command:    strings.Join(command.Args, " "),
		output:     output,
		err:        err,
		contextErr: ctx.Err(),
	}
}

func isVolumeMountCapabilityError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

func TestAliasCommandContext_UsesHelperDeadline(t *testing.T) {
	startedAt := time.Now()
	ctx, cancel := aliasCommandContext(context.Background())
	defer cancel()
	createdAt := time.Now()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("alias command context has no deadline")
	}
	if deadline.Before(startedAt.Add(helperDeadline)) ||
		deadline.After(createdAt.Add(helperDeadline)) {
		t.Fatalf(
			"alias command deadline = %v, want between %v and %v",
			deadline,
			startedAt.Add(helperDeadline),
			createdAt.Add(helperDeadline),
		)
	}
}

func TestRunAliasCommand_CanceledContextIsReported(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runAliasCommand(ctx, nil, executable)
	if result.err == nil {
		t.Fatal("runAliasCommand() error = nil, want canceled context")
	}
	if !errors.Is(result.contextErr, context.Canceled) {
		t.Fatalf(
			"runAliasCommand() context error = %v, want %v",
			result.contextErr,
			context.Canceled,
		)
	}
	if result.command == "" {
		t.Fatal("runAliasCommand() command = empty, want diagnostic command")
	}
}

func TestVolumeMountCapabilityError_Whitelist(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "access denied",
			err:  windows.ERROR_ACCESS_DENIED,
			want: true,
		},
		{
			name: "not supported",
			err:  windows.ERROR_NOT_SUPPORTED,
			want: true,
		},
		{
			name: "privilege not held",
			err:  windows.ERROR_PRIVILEGE_NOT_HELD,
			want: true,
		},
		{
			name: "wrapped access denied",
			err:  fmt.Errorf("query volume: %w", windows.ERROR_ACCESS_DENIED),
			want: true,
		},
		{
			name: "invalid parameter",
			err:  windows.ERROR_INVALID_PARAMETER,
			want: false,
		},
		{
			name: "unclassified",
			err:  errors.New("unexpected volume failure"),
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := isVolumeMountCapabilityError(test.err)
			if got != test.want {
				t.Fatalf(
					"isVolumeMountCapabilityError(%v) = %t, want %t",
					test.err,
					got,
					test.want,
				)
			}
		})
	}
}

type helperCommand struct {
	Type string `json:"type"`
}

type helperMessage struct {
	Type      string        `json:"type"`
	Token     string        `json:"token,omitempty"`
	Success   bool          `json:"success,omitempty"`
	Code      protocol.Code `json:"code,omitempty"`
	Recovered bool          `json:"recovered,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type helperProcess struct {
	command  *exec.Cmd
	cancel   context.CancelFunc
	conn     net.Conn
	encoder  *json.Encoder
	decoder  *json.Decoder
	stderr   *lockedBuffer
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func newHelperToken(t *testing.T) string {
	t.Helper()
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return hex.EncodeToString(value[:])
}

func startLockHelper(
	t *testing.T,
	appRoot string,
	kind Kind,
	mode string,
) *helperProcess {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil &&
			!errors.Is(err, net.ErrClosed) {
			t.Errorf("cleanup listener.Close() error = %v", err)
		}
	})
	tcpListener := listener.(*net.TCPListener)
	if err := tcpListener.SetDeadline(time.Now().Add(helperDeadline)); err != nil {
		t.Fatalf("listener.SetDeadline() error = %v", err)
	}

	token := newHelperToken(t)
	stderr := &lockedBuffer{}
	ctx, cancel := context.WithTimeout(t.Context(), 4*helperDeadline)
	command := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^TestLockHelperProcess$",
		"-test.v",
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_LOCK_HELPER=1",
		"AUTO_MAS_LOCK_HELPER_ADDR="+listener.Addr().String(),
		"AUTO_MAS_LOCK_HELPER_TOKEN="+token,
		"AUTO_MAS_LOCK_HELPER_ROOT="+appRoot,
		"AUTO_MAS_LOCK_HELPER_KIND="+kind.String(),
		"AUTO_MAS_LOCK_HELPER_MODE="+mode,
	)
	command.Stdout = io.Discard
	command.Stderr = stderr
	helper := &helperProcess{
		command:  command,
		cancel:   cancel,
		stderr:   stderr,
		waitDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		helper.cleanup(t)
	})
	if err := command.Start(); err != nil {
		t.Fatalf("helper Start() error = %v", err)
	}

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("helper Accept() error = %v; stderr=%q", err, stderr.String())
	}
	helper.conn = conn
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}
	helper.encoder = json.NewEncoder(conn)
	helper.decoder = json.NewDecoder(conn)

	hello := helper.receive(t, "HELLO")
	if hello.Type != "HELLO" || hello.Token != token {
		t.Fatalf("helper HELLO = %#v, want matching token", hello)
	}
	return helper
}

func (h *helperProcess) send(t *testing.T, messageType string) {
	t.Helper()
	if err := h.conn.SetWriteDeadline(
		time.Now().Add(helperDeadline),
	); err != nil {
		t.Fatalf("helper set write deadline error = %v", err)
	}
	if err := h.encoder.Encode(helperCommand{Type: messageType}); err != nil {
		t.Fatalf("helper send %q error = %v", messageType, err)
	}
}

func (h *helperProcess) receive(t *testing.T, wantType string) helperMessage {
	t.Helper()
	if err := h.conn.SetReadDeadline(
		time.Now().Add(helperDeadline),
	); err != nil {
		t.Fatalf("helper set read deadline error = %v", err)
	}
	var message helperMessage
	if err := h.decoder.Decode(&message); err != nil {
		t.Fatalf(
			"helper receive %q error = %v; stderr=%q",
			wantType,
			err,
			h.stderr.String(),
		)
	}
	if message.Type != wantType {
		t.Fatalf("helper message type = %q, want %q", message.Type, wantType)
	}
	return message
}

func (h *helperProcess) reap() error {
	h.waitOnce.Do(func() {
		if h.command.Process != nil {
			h.waitErr = h.command.Wait()
		}
		close(h.waitDone)
	})
	return h.waitErr
}

func (h *helperProcess) wait(t *testing.T) {
	t.Helper()
	if err := h.reap(); err != nil {
		t.Fatalf("helper Wait() error = %v; stderr=%q", err, h.stderr.String())
	}
	if h.command.ProcessState == nil {
		t.Fatalf("helper ProcessState = nil after Wait; stderr=%q", h.stderr.String())
	}
	h.closeTransport(t)
}

func (h *helperProcess) killAndWait(t *testing.T) {
	t.Helper()
	if h.command.Process == nil {
		t.Fatal("helper Process = nil, want started process")
	}
	killErr := h.command.Process.Kill()
	waitErr := h.reap() // Kill 的每条结果路径都必须 Wait/reap。
	if killErr != nil {
		t.Fatalf(
			"helper Kill() error = %v; Wait error = %v; stderr=%q",
			killErr,
			waitErr,
			h.stderr.String(),
		)
	}
	if h.command.ProcessState == nil {
		t.Fatalf("helper ProcessState = nil after Kill/Wait; stderr=%q", h.stderr.String())
	}
	h.closeTransport(t)
}

func (h *helperProcess) closeTransport(t *testing.T) {
	t.Helper()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil &&
			!errors.Is(err, net.ErrClosed) {
			t.Errorf("helper conn.Close() error = %v", err)
		}
	}
	h.cancel()
}

func (h *helperProcess) cleanup(t *testing.T) {
	t.Helper()
	h.cancel()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil &&
			!errors.Is(err, net.ErrClosed) {
			t.Errorf("cleanup conn.Close() error = %v", err)
		}
	}
	if h.command.Process == nil {
		return
	}
	select {
	case <-h.waitDone:
	default:
		killErr := h.command.Process.Kill()
		waitErr := h.reap()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("cleanup Process.Kill() error = %v", killErr)
		}
		if waitErr == nil && killErr == nil {
			t.Errorf("cleanup killed an unreleased helper without an exit error")
		}
	}
	if h.command.Process != nil && h.command.ProcessState == nil {
		t.Errorf("lock helper was not reaped; stderr=%q", h.stderr.String())
	}
}

type peerBarrierAPI struct {
	windowsAPI
	mu          sync.Mutex
	waits       int
	entered     chan struct{}
	probe       chan struct{}
	returned    chan struct{}
	cleanup     chan struct{}
	probeOnce   sync.Once
	cleanupOnce sync.Once
}

func (a *peerBarrierAPI) waitForSingleObject(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (uint32, error) {
	a.mu.Lock()
	a.waits++
	waitNumber := a.waits
	a.mu.Unlock()
	if waitNumber != 2 {
		return a.windowsAPI.waitForSingleObject(
			handle,
			timeoutMilliseconds,
		)
	}
	close(a.entered)
	<-a.probe
	result, err := a.windowsAPI.waitForSingleObject(
		handle,
		timeoutMilliseconds,
	)
	close(a.returned)
	<-a.cleanup
	return result, err
}

func (a *peerBarrierAPI) continueProbe() {
	a.probeOnce.Do(func() { close(a.probe) })
}

func (a *peerBarrierAPI) continueCleanup() {
	a.cleanupOnce.Do(func() { close(a.cleanup) })
}

func (a *peerBarrierAPI) unblock() {
	a.continueProbe()
	a.continueCleanup()
}

type helperWire struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
}

func (w *helperWire) send(message helperMessage) error {
	if err := w.conn.SetWriteDeadline(
		time.Now().Add(helperDeadline),
	); err != nil {
		return fmt.Errorf("set helper write deadline: %w", err)
	}
	if err := w.encoder.Encode(message); err != nil {
		return fmt.Errorf("encode helper message %q: %w", message.Type, err)
	}
	return nil
}

func (w *helperWire) receive(want string) (helperCommand, error) {
	if err := w.conn.SetReadDeadline(
		time.Now().Add(helperDeadline),
	); err != nil {
		return helperCommand{}, fmt.Errorf(
			"set helper read deadline: %w",
			err,
		)
	}
	var command helperCommand
	if err := w.decoder.Decode(&command); err != nil {
		return helperCommand{}, fmt.Errorf(
			"decode helper command %q: %w",
			want,
			err,
		)
	}
	if command.Type != want {
		return helperCommand{}, fmt.Errorf(
			"helper command = %q, want %q",
			command.Type,
			want,
		)
	}
	return command, nil
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv("AUTO_MAS_LOCK_HELPER") != "1" {
		return
	}
	if err := runLockHelper(t.Context()); err != nil {
		t.Fatalf("runLockHelper() error = %v", err)
	}
}

func runLockHelper(ctx context.Context) (returnErr error) {
	address := os.Getenv("AUTO_MAS_LOCK_HELPER_ADDR")
	token := os.Getenv("AUTO_MAS_LOCK_HELPER_TOKEN")
	root := os.Getenv("AUTO_MAS_LOCK_HELPER_ROOT")
	kind := Kind(os.Getenv("AUTO_MAS_LOCK_HELPER_KIND"))
	mode := os.Getenv("AUTO_MAS_LOCK_HELPER_MODE")
	if address == "" || token == "" || root == "" {
		return errors.New("helper environment is incomplete")
	}
	if !kind.Valid() {
		return fmt.Errorf("helper mutex kind = %q", kind)
	}
	if mode != "hold" && mode != "cross" {
		return fmt.Errorf("helper mode = %q", mode)
	}

	conn, err := net.DialTimeout("tcp4", address, helperDeadline)
	if err != nil {
		return fmt.Errorf("dial helper control: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, conn.Close())
	}()
	wire := &helperWire{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
	}
	if err := wire.send(helperMessage{
		Type:  "HELLO",
		Token: token,
	}); err != nil {
		return err
	}

	layout, err := config.NewLayout(root, filepath.Dir(filepath.Clean(root)))
	if err != nil {
		return fmt.Errorf("new helper layout: %w", err)
	}
	api := windowsAPI(systemWindowsAPI{})
	var barrier *peerBarrierAPI
	if mode == "cross" {
		barrier = &peerBarrierAPI{
			windowsAPI: api,
			entered:    make(chan struct{}),
			probe:      make(chan struct{}),
			returned:   make(chan struct{}),
			cleanup:    make(chan struct{}),
		}
		api = barrier
	}
	set, err := newSet(ctx, layout, api)
	if err != nil {
		return fmt.Errorf("new helper set: %w", err)
	}
	defer func() {
		if barrier != nil {
			barrier.unblock()
		}
		returnErr = errors.Join(returnErr, set.Close())
	}()

	acquired := make(chan workerResponse, 1)
	go func() {
		var result AcquisitionResult
		var acquireErr error
		if kind == KindBackend {
			result, acquireErr = set.AcquireBackend(ctx)
		} else {
			result, acquireErr = set.AcquireMutation(ctx)
		}
		acquired <- workerResponse{acquisition: result, err: acquireErr}
	}()

	waitAcquired := func() (workerResponse, error) {
		select {
		case response := <-acquired:
			return response, nil
		case <-ctx.Done():
			return workerResponse{}, ctx.Err()
		}
	}
	sendResult := func(
		response workerResponse,
		extraErr error,
	) error {
		resultErr := errors.Join(response.err, extraErr)
		message := helperMessage{
			Type:      "RESULT",
			Success:   resultErr == nil,
			Recovered: response.acquisition.Recovered(),
		}
		if resultErr == nil {
			message.Code = protocol.CodeOK
		} else {
			var coded interface{ Code() protocol.Code }
			if errors.As(resultErr, &coded) {
				message.Code = coded.Code()
			}
			message.Error = resultErr.Error()
		}
		return wire.send(message)
	}

	if barrier == nil {
		response, err := waitAcquired()
		if err != nil {
			return err
		}
		lease := response.acquisition.Lease()
		if lease == nil {
			return sendResult(response, nil)
		}
		if err := wire.send(helperMessage{
			Type:      "READY",
			Success:   true,
			Recovered: response.acquisition.Recovered(),
		}); err != nil {
			return err
		}
		if _, err := wire.receive("RELEASE"); err != nil {
			return err
		}
		releaseErr := lease.Close()
		if err := sendResult(response, releaseErr); err != nil {
			return err
		}
		return releaseErr
	}

	select {
	case <-barrier.entered:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := wire.send(helperMessage{Type: "READY", Success: true}); err != nil {
		return err
	}
	if _, err := wire.receive("CONTINUE"); err != nil {
		return err
	}
	barrier.continueProbe()

	select {
	case <-barrier.returned:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := wire.send(helperMessage{Type: "PROBED"}); err != nil {
		return err
	}
	if _, err := wire.receive("CLEANUP"); err != nil {
		return err
	}
	barrier.continueCleanup()

	response, err := waitAcquired()
	if err != nil {
		return err
	}
	var releaseErr error
	if lease := response.acquisition.Lease(); lease != nil {
		releaseErr = lease.Close()
	}
	if err := sendResult(response, releaseErr); err != nil {
		return err
	}
	return releaseErr
}

func assertHelperSuccess(t *testing.T, message helperMessage) {
	t.Helper()
	if !message.Success ||
		message.Code != protocol.CodeOK ||
		message.Error != "" {
		t.Fatalf("helper result = %#v, want success", message)
	}
}

func assertHelperConflict(
	t *testing.T,
	message helperMessage,
	want protocol.Code,
) {
	t.Helper()
	if message.Success || message.Code != want || message.Error == "" {
		t.Fatalf("helper result = %#v, want conflict %q", message, want)
	}
}

func releaseAndWait(
	t *testing.T,
	helper *helperProcess,
) helperMessage {
	t.Helper()
	helper.send(t, "RELEASE")
	result := helper.receive(t, "RESULT")
	assertHelperSuccess(t, result)
	helper.wait(t)
	return result
}

func assertRootFree(t *testing.T, root string) {
	t.Helper()
	layout, err := config.NewLayout(
		root,
		filepath.Dir(filepath.Clean(root)),
	)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	set, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Errorf("cleanup Set.Close() error = %v", err)
		}
	})
	for _, kind := range []Kind{KindBackend, KindMutation} {
		result, err := set.Probe(t.Context(), kind)
		if err != nil {
			t.Fatalf("Probe(%q) error = %v", kind, err)
		}
		if result.Held {
			t.Fatalf("Probe(%q).Held = true, want false", kind)
		}
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close() error = %v", err)
	}
}

func TestSet_TwoProcessesSameRootConflict(t *testing.T) {
	root := t.TempDir()
	owner := startLockHelper(t, root, KindBackend, "hold")
	ready := owner.receive(t, "READY")
	if !ready.Success {
		t.Fatalf("owner READY = %#v, want success", ready)
	}

	contender := startLockHelper(t, root, KindBackend, "hold")
	conflict := contender.receive(t, "RESULT")
	assertHelperConflict(
		t,
		conflict,
		protocol.CodeBackendAlreadyRunning,
	)
	contender.wait(t)
	releaseAndWait(t, owner)
	assertRootFree(t, root)
}

func TestSet_TwoProcessesDifferentRootsDoNotConflict(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := startLockHelper(t, firstRoot, KindBackend, "hold")
	second := startLockHelper(t, secondRoot, KindBackend, "hold")
	if ready := first.receive(t, "READY"); !ready.Success {
		t.Fatalf("first READY = %#v, want success", ready)
	}
	if ready := second.receive(t, "READY"); !ready.Success {
		t.Fatalf("second READY = %#v, want success", ready)
	}
	releaseAndWait(t, first)
	releaseAndWait(t, second)
	assertRootFree(t, firstRoot)
	assertRootFree(t, secondRoot)
}

func TestSet_AbandonedHelperRecovers(t *testing.T) {
	root := t.TempDir()
	victim := startLockHelper(t, root, KindBackend, "hold")
	if ready := victim.receive(t, "READY"); ready.Recovered {
		t.Fatalf("victim READY = %#v, want fresh acquisition", ready)
	}

	layout, err := config.NewLayout(
		root,
		filepath.Dir(filepath.Clean(root)),
	)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	observerAPI := &recordingWindowsAPI{windowsAPI: systemWindowsAPI{}}
	observer, err := newSet(t.Context(), layout, observerAPI)
	if err != nil {
		t.Fatalf("observer newSet() error = %v", err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Errorf("cleanup observer Set.Close() error = %v", err)
		}
	})

	// observer 在 Kill 前已打开同名对象，避免最后一个 handle 消失后对象被销毁。
	victim.killAndWait(t)
	recovered, err := observer.AcquireBackend(t.Context())
	if err != nil {
		t.Fatalf("observer AcquireBackend() error = %v", err)
	}
	if !recovered.Recovered() || recovered.Lease() == nil {
		t.Fatalf(
			"observer acquisition = (recovered=%t, lease=%v), want true/non-nil",
			recovered.Recovered(),
			recovered.Lease(),
		)
	}
	waits := observerAPI.recordedWaits()
	if len(waits) == 0 ||
		waits[0].timeout != mutexWaitTimeout ||
		waits[0].result != waitResultAbandoned ||
		waits[0].err != nil {
		t.Fatalf(
			"observer first zero Wait = %#v, want WAIT_ABANDONED",
			waits,
		)
	}
	if err := recovered.Lease().Close(); err != nil {
		t.Fatalf("observer Lease.Close() error = %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("observer Set.Close() error = %v", err)
	}
	assertRootFree(t, root)
}

func TestSet_NormalHelperReleaseIsNotAbandoned(t *testing.T) {
	root := t.TempDir()
	first := startLockHelper(t, root, KindBackend, "hold")
	if ready := first.receive(t, "READY"); ready.Recovered {
		t.Fatalf("first READY = %#v, want Recovered=false", ready)
	}
	releaseAndWait(t, first)

	second := startLockHelper(t, root, KindBackend, "hold")
	ready := second.receive(t, "READY")
	if !ready.Success || ready.Recovered {
		t.Fatalf("second READY = %#v, want non-abandoned success", ready)
	}
	releaseAndWait(t, second)
	assertRootFree(t, root)
}

func TestSet_TwoProcessesCrossAcquireBothRelease(t *testing.T) {
	root := t.TempDir()
	backend := startLockHelper(t, root, KindBackend, "cross")
	mutation := startLockHelper(t, root, KindMutation, "cross")

	backend.receive(t, "READY")
	mutation.receive(t, "READY")
	backend.send(t, "CONTINUE")
	mutation.send(t, "CONTINUE")

	backend.receive(t, "PROBED")
	mutation.receive(t, "PROBED")
	backend.send(t, "CLEANUP")
	mutation.send(t, "CLEANUP")

	backendResult := backend.receive(t, "RESULT")
	mutationResult := mutation.receive(t, "RESULT")
	assertHelperConflict(
		t,
		backendResult,
		protocol.CodeMutationInProgress,
	)
	assertHelperConflict(
		t,
		mutationResult,
		protocol.CodeBackendStillRunning,
	)
	backend.wait(t)
	mutation.wait(t)

	third := startLockHelper(t, root, KindBackend, "hold")
	if ready := third.receive(t, "READY"); !ready.Success {
		t.Fatalf("third READY = %#v, want success", ready)
	}
	releaseAndWait(t, third)
	assertRootFree(t, root)
}

func assertAliasSharesMutex(
	t *testing.T,
	root string,
	alias string,
) {
	t.Helper()
	owner := startLockHelper(t, root, KindBackend, "hold")
	if ready := owner.receive(t, "READY"); !ready.Success {
		t.Fatalf("owner READY = %#v, want success", ready)
	}
	contender := startLockHelper(t, alias, KindBackend, "hold")
	conflict := contender.receive(t, "RESULT")
	assertHelperConflict(
		t,
		conflict,
		protocol.CodeBackendAlreadyRunning,
	)
	contender.wait(t)
	releaseAndWait(t, owner)
	assertRootFree(t, root)
}

func junctionAlias(t *testing.T, target string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "app-root-junction")
	script := `$ErrorActionPreference = 'Stop'
$target = $env:AUTO_MAS_JUNCTION_TARGET
$path = $env:AUTO_MAS_JUNCTION_ALIAS
if ([string]::IsNullOrWhiteSpace($target) -or
    [string]::IsNullOrWhiteSpace($path)) {
    throw 'junction environment is incomplete'
}
try {
    New-Item -ItemType Junction -Path $path -Target $target -ErrorAction Stop |
        Out-Null
} catch {
    $exception = $_.Exception
    $nativeCode = $exception.HResult -band 0xffff
    $category = $_.CategoryInfo.Category
    $skippable = (
        $exception -is [System.UnauthorizedAccessException] -or
        $exception -is [System.PlatformNotSupportedException] -or
        $exception -is [System.NotSupportedException] -or
        $category -eq [System.Management.Automation.ErrorCategory]::PermissionDenied -or
        $category -eq [System.Management.Automation.ErrorCategory]::SecurityError -or
        $nativeCode -in 5, 50, 1314
    )
    [Console]::Error.WriteLine(
        ('junction create failed: category={0}; hresult=0x{1:X8}; native={2}; message={3}' -f
            $category, $exception.HResult, $nativeCode, $exception.Message)
    )
    if ($skippable) {
        exit 77
    }
    throw
}`
	result := runAliasCommand(
		t.Context(),
		append(
			os.Environ(),
			"AUTO_MAS_JUNCTION_TARGET="+target,
			"AUTO_MAS_JUNCTION_ALIAS="+alias,
		),
		"pwsh",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	)
	if result.err != nil {
		var exitErr *exec.ExitError
		if errors.As(result.err, &exitErr) && exitErr.ExitCode() == 77 {
			t.Skipf(
				"Junction permission or capability unavailable: %v; output=%q",
				result.err,
				result.output,
			)
		}
		t.Fatalf(
			"create Junction command=%q error=%v; context error=%v; output=%q",
			result.command,
			result.err,
			result.contextErr,
			result.output,
		)
	}
	t.Cleanup(func() {
		// os.Remove 删除 Junction 自身且不递归跟随目标。
		if err := os.Remove(alias); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove Junction error = %v", err)
		}
	})
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("os.Stat(Junction target) error = %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatalf("os.Stat(Junction alias) error = %v", err)
	}
	if !os.SameFile(targetInfo, aliasInfo) {
		t.Fatal("Junction alias and target do not identify the same directory")
	}
	return alias
}

func shortPathAlias(t *testing.T, target string) string {
	t.Helper()
	longName, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v", err)
	}
	size, err := windows.GetShortPathName(longName, nil, 0)
	if err != nil || size == 0 {
		t.Skipf("8.3 capability unavailable: size=%d error=%v", size, err)
	}
	buffer := make([]uint16, size)
	length, err := windows.GetShortPathName(
		longName,
		&buffer[0],
		uint32(len(buffer)),
	)
	if err != nil {
		t.Skipf("8.3 capability unavailable: %v", err)
	}
	alias := windows.UTF16ToString(buffer[:length])
	if strings.EqualFold(filepath.Clean(alias), filepath.Clean(target)) {
		t.Skip("8.3 short names are disabled or target has no short alias")
	}
	return alias
}

func substAlias(t *testing.T, target string) string {
	t.Helper()
	cleanTarget := filepath.Clean(target)
	base := filepath.Dir(cleanTarget)
	leaf := filepath.Base(cleanTarget)
	if base == cleanTarget || leaf == "." || leaf == string(os.PathSeparator) {
		t.Fatalf("SUBST target %q has no dedicated parent/leaf", target)
	}
	targetInfo, err := os.Stat(cleanTarget)
	if err != nil {
		t.Fatalf("os.Stat(SUBST target) error = %v", err)
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		t.Fatalf("os.Stat(SUBST base) error = %v", err)
	}
	if !targetInfo.IsDir() || !baseInfo.IsDir() {
		t.Fatalf(
			"SUBST target/base directories = %t/%t, want true/true",
			targetInfo.IsDir(),
			baseInfo.IsDir(),
		)
	}
	substPath, err := exec.LookPath("subst.exe")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skipf("SUBST tool unavailable: exec.LookPath: %v", err)
		}
		t.Fatalf("exec.LookPath(subst.exe) error = %v", err)
	}
	var freeDrives []string
	for drive := 'Z'; drive >= 'D'; drive-- {
		driveName := fmt.Sprintf("%c:", drive)
		_, statErr := os.Stat(driveName + `\`)
		switch {
		case statErr == nil:
			continue
		case errors.Is(statErr, os.ErrNotExist):
			freeDrives = append(freeDrives, driveName)
		default:
			t.Fatalf("os.Stat(%s\\) error = %v", driveName, statErr)
		}
	}
	if len(freeDrives) == 0 {
		t.Skip("SUBST capability unavailable: no free drive letter")
	}
	mappedDrive := freeDrives[0]
	result := runAliasCommand(
		t.Context(),
		nil,
		substPath,
		mappedDrive,
		base,
	)
	if result.err != nil {
		t.Fatalf(
			"create SUBST %s command=%q error=%v; context error=%v; output=%q",
			mappedDrive,
			result.command,
			result.err,
			result.contextErr,
			result.output,
		)
	}
	if len(bytes.TrimSpace(result.output)) != 0 {
		t.Fatalf(
			"create SUBST %s output = %q, want empty",
			mappedDrive,
			result.output,
		)
	}
	t.Cleanup(func() {
		cleanupResult := runAliasCommand(
			context.Background(),
			nil,
			substPath,
			mappedDrive,
			"/D",
		)
		if cleanupResult.err != nil {
			t.Fatalf(
				"remove SUBST %s command=%q error=%v; context error=%v; output=%q",
				mappedDrive,
				cleanupResult.command,
				cleanupResult.err,
				cleanupResult.contextErr,
				cleanupResult.output,
			)
			return
		}
		if len(bytes.TrimSpace(cleanupResult.output)) != 0 {
			t.Fatalf(
				"remove SUBST %s output = %q, want empty",
				mappedDrive,
				cleanupResult.output,
			)
		}
		if _, err := os.Stat(mappedDrive + `\`); err == nil {
			t.Errorf("SUBST %s still exists after cleanup", mappedDrive)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Errorf(
				"stat removed SUBST %s error = %v",
				mappedDrive,
				err,
			)
		}
		if _, err := os.Stat(cleanTarget); err != nil {
			t.Errorf("SUBST target after cleanup error = %v", err)
		}
	})
	alias := filepath.Join(mappedDrive+`\`, leaf)
	if filepath.Clean(alias) == filepath.Clean(mappedDrive+`\`) {
		t.Fatalf("SUBST alias = %q, want a child of the drive root", alias)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatalf("os.Stat(SUBST alias) error = %v", err)
	}
	if !os.SameFile(targetInfo, aliasInfo) {
		t.Fatalf(
			"SUBST alias %q and target %q do not identify the same directory",
			alias,
			cleanTarget,
		)
	}
	return alias
}

func volumeMountAlias(t *testing.T, target string) string {
	t.Helper()
	const bufferLength = 32768
	targetName, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(target) error = %v", err)
	}
	volumeRootBuffer := make([]uint16, bufferLength)
	if err := windows.GetVolumePathName(
		targetName,
		&volumeRootBuffer[0],
		uint32(len(volumeRootBuffer)),
	); err != nil {
		if isVolumeMountCapabilityError(err) {
			t.Skipf(
				"volume mount capability unavailable: GetVolumePathName: %v",
				err,
			)
		}
		t.Fatalf("GetVolumePathName() error = %v", err)
	}
	volumeRoot := windows.UTF16ToString(volumeRootBuffer)
	volumeRootName, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(volume root) error = %v", err)
	}
	volumeNameBuffer := make([]uint16, bufferLength)
	if err := windows.GetVolumeNameForVolumeMountPoint(
		volumeRootName,
		&volumeNameBuffer[0],
		uint32(len(volumeNameBuffer)),
	); err != nil {
		if isVolumeMountCapabilityError(err) {
			t.Skipf(
				"volume mount capability unavailable: GetVolumeNameForVolumeMountPoint: %v",
				err,
			)
		}
		t.Fatalf("GetVolumeNameForVolumeMountPoint() error = %v", err)
	}
	volumeName := windows.UTF16ToString(volumeNameBuffer)

	mountPath := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(mountPath, 0o700); err != nil {
		t.Fatalf("os.Mkdir(volume mount) error = %v", err)
	}
	mountPoint := mountPath + string(os.PathSeparator)
	mountPointName, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(mount point) error = %v", err)
	}
	volumeNamePtr, err := windows.UTF16PtrFromString(volumeName)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(volume name) error = %v", err)
	}
	if err := windows.SetVolumeMountPoint(
		mountPointName,
		volumeNamePtr,
	); err != nil {
		if isVolumeMountCapabilityError(err) {
			t.Skipf(
				"volume mount capability unavailable: SetVolumeMountPoint: %v",
				err,
			)
		}
		t.Fatalf("SetVolumeMountPoint() error = %v", err)
	}
	t.Cleanup(func() {
		if err := windows.DeleteVolumeMountPoint(mountPointName); err != nil {
			t.Errorf("DeleteVolumeMountPoint() error = %v", err)
		}
		if err := os.Remove(mountPath); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove volume mount directory error = %v", err)
		}
	})
	relative, err := filepath.Rel(volumeRoot, target)
	if err != nil {
		t.Fatalf("filepath.Rel(volume root, target) error = %v", err)
	}
	return filepath.Join(mountPath, relative)
}

func TestSet_PhysicalRootAliasesShareMutex(t *testing.T) {
	tests := []struct {
		name  string
		alias func(*testing.T, string) string
	}{
		{
			name: "case",
			alias: func(t *testing.T, root string) string {
				t.Helper()
				alias := strings.ToUpper(root)
				if _, err := os.Stat(alias); err != nil {
					t.Skipf("case alias unavailable: %v", err)
				}
				return alias
			},
		},
		{
			name: "trailing separator",
			alias: func(t *testing.T, root string) string {
				t.Helper()
				return root + string(os.PathSeparator)
			},
		},
		{name: "junction", alias: junctionAlias},
		{name: "8.3", alias: shortPathAlias},
		{name: "SUBST", alias: substAlias},
		{name: "volume mount", alias: volumeMountAlias},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "AppRoot")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatalf("os.Mkdir(app root) error = %v", err)
			}
			alias := test.alias(t, root)
			assertAliasSharesMutex(t, root, alias)
		})
	}
}

func TestSet_IntegrationLeavesNoResources(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []Kind{KindBackend, KindMutation} {
		helper := startLockHelper(t, root, kind, "hold")
		if ready := helper.receive(t, "READY"); !ready.Success {
			t.Fatalf("helper %q READY = %#v, want success", kind, ready)
		}
		releaseAndWait(t, helper)
	}
	assertRootFree(t, root)
}
