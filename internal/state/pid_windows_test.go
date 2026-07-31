//go:build windows

package state

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const fakeProcessHandle = windows.Handle(0x1234)

func completeFakeProcessAPI() processAPI {
	return processAPI{
		openProcess: func(
			uint32,
			bool,
			uint32,
		) (windows.Handle, error) {
			return fakeProcessHandle, nil
		},
		waitForSingleObject: func(
			windows.Handle,
			uint32,
		) (uint32, error) {
			return uint32(windows.WAIT_TIMEOUT), nil
		},
		closeHandle: func(windows.Handle) error {
			return nil
		},
	}
}

func TestSystemPIDProbe_CurrentProcessIsAlive(t *testing.T) {
	t.Parallel()

	probe := NewSystemPIDProbe()
	alive, err := probe.Alive(t.Context(), uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("Alive(current PID) error = %v", err)
	}
	if !alive {
		t.Fatal("Alive(current PID) = false, want true")
	}
}

func TestSystemPIDProbe_ExitedHelperIsDead(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("listener Close() error = %v", err)
		}
	})
	tcpListener := listener.(*net.TCPListener)
	if err := tcpListener.SetDeadline(time.Now().Add(testBarrierTimeout)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	token := hex.EncodeToString(random[:])
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestSystemPIDProbe_HelperProcess$",
		"-test.v",
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_PID_HELPER=1",
		"AUTO_MAS_PID_HELPER_ADDR="+listener.Addr().String(),
		"AUTO_MAS_PID_HELPER_TOKEN="+token,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("helper Start() error = %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		// 测试失败时只回收本测试启动且尚未 Wait 的 helper。
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	connection, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("connection Close() error = %v", err)
		}
	})
	if err := connection.SetDeadline(time.Now().Add(testBarrierTimeout)); err != nil {
		t.Fatalf("connection SetDeadline() error = %v", err)
	}
	reader := bufio.NewReader(connection)
	ready, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read READY error = %v", err)
	}
	if strings.TrimSpace(ready) != token+" READY" {
		t.Fatalf("helper READY = %q, want authenticated READY", ready)
	}

	probe := NewSystemPIDProbe()
	pid := uint32(command.Process.Pid)
	alive, err := probe.Alive(t.Context(), pid)
	if err != nil || !alive {
		t.Fatalf("Alive(helper before EXIT) = %t, %v, want true/nil", alive, err)
	}
	if _, err := fmt.Fprintf(connection, "%s EXIT\n", token); err != nil {
		t.Fatalf("write EXIT error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper Wait() error = %v", err)
	}
	waited = true
	alive, err = probe.Alive(t.Context(), pid)
	if err != nil {
		t.Fatalf("Alive(helper after Wait) error = %v", err)
	}
	if alive {
		t.Fatal("Alive(helper after Wait) = true, want false")
	}
}

func TestSystemPIDProbe_HelperProcess(t *testing.T) {
	if os.Getenv("AUTO_MAS_PID_HELPER") != "1" {
		t.Skip("helper entrypoint")
	}
	address := os.Getenv("AUTO_MAS_PID_HELPER_ADDR")
	token := os.Getenv("AUTO_MAS_PID_HELPER_TOKEN")
	if address == "" || token == "" {
		t.Fatal("helper environment is incomplete")
	}
	connection, err := net.DialTimeout("tcp4", address, testBarrierTimeout)
	if err != nil {
		t.Fatalf("helper DialTimeout() error = %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("helper connection Close() error = %v", err)
		}
	})
	if err := connection.SetDeadline(time.Now().Add(testBarrierTimeout)); err != nil {
		t.Fatalf("helper SetDeadline() error = %v", err)
	}
	if _, err := fmt.Fprintf(connection, "%s READY\n", token); err != nil {
		t.Fatalf("helper write READY error = %v", err)
	}
	exit, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatalf("helper read EXIT error = %v", err)
	}
	if strings.TrimSpace(exit) != token+" EXIT" {
		t.Fatalf("helper EXIT = %q, want authenticated EXIT", exit)
	}
}

func TestSystemPIDProbe_NotFoundIsDead(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_NOT_FOUND,
	} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			api := completeFakeProcessAPI()
			waitCalls := 0
			closeCalls := 0
			api.openProcess = func(
				uint32,
				bool,
				uint32,
			) (windows.Handle, error) {
				return 0, cause
			}
			api.waitForSingleObject = func(
				windows.Handle,
				uint32,
			) (uint32, error) {
				waitCalls++
				return windows.WAIT_FAILED, errors.New("unexpected wait")
			}
			api.closeHandle = func(windows.Handle) error {
				closeCalls++
				return nil
			}
			alive, err := newSystemPIDProbeWith(api).Alive(t.Context(), 4242)
			if err != nil {
				t.Fatalf("Alive(not found) error = %v", err)
			}
			if alive {
				t.Fatal("Alive(not found) = true, want false")
			}
			if waitCalls != 0 || closeCalls != 0 {
				t.Fatalf(
					"wait/close calls = %d/%d, want 0/0",
					waitCalls,
					closeCalls,
				)
			}
		})
	}
}

func TestSystemPIDProbe_ContextWinsOverOpenProcessFailure(t *testing.T) {
	t.Parallel()

	for _, openErr := range []error{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_ACCESS_DENIED,
		errors.New("other open failure"),
	} {
		openErr := openErr
		t.Run(openErr.Error(), func(t *testing.T) {
			openEntered := make(chan struct{})
			releaseOpen := make(chan struct{})
			waitCalls := 0
			closeCalls := 0
			api := completeFakeProcessAPI()
			api.openProcess = func(
				uint32,
				bool,
				uint32,
			) (windows.Handle, error) {
				close(openEntered)
				<-releaseOpen
				return 0, openErr
			}
			api.waitForSingleObject = func(
				windows.Handle,
				uint32,
			) (uint32, error) {
				waitCalls++
				return windows.WAIT_FAILED, errors.New("unexpected wait")
			}
			api.closeHandle = func(windows.Handle) error {
				closeCalls++
				return nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				_, err := newSystemPIDProbeWith(api).Alive(ctx, 4242)
				result <- err
			}()
			waitForTestSignal(t, openEntered, "OpenProcess barrier")
			cancel()
			close(releaseOpen)
			err := receiveTestValue(t, result, "PID open failure result")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Alive() error = %v, want context.Canceled", err)
			}
			var probeErr *PIDProbeError
			if errors.As(err, &probeErr) {
				t.Fatalf("Alive() error = %v, context must win before open classification", err)
			}
			if waitCalls != 0 || closeCalls != 0 {
				t.Fatalf("wait/close calls = %d/%d, want 0/0", waitCalls, closeCalls)
			}
		})
	}
}

func TestSystemPIDProbe_ContextAfterOpenSkipsWaitAndClosesHandle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	closeCause := errors.New("close after canceled open")
	waitCalls := 0
	closeCalls := 0
	api := completeFakeProcessAPI()
	api.openProcess = func(
		uint32,
		bool,
		uint32,
	) (windows.Handle, error) {
		cancel()
		return fakeProcessHandle, nil
	}
	api.waitForSingleObject = func(
		windows.Handle,
		uint32,
	) (uint32, error) {
		waitCalls++
		return uint32(windows.WAIT_TIMEOUT), nil
	}
	api.closeHandle = func(handle windows.Handle) error {
		closeCalls++
		if handle != fakeProcessHandle {
			t.Fatalf("CloseHandle handle = %#x, want %#x", handle, fakeProcessHandle)
		}
		return closeCause
	}

	alive, err := newSystemPIDProbeWith(api).Alive(ctx, 4242)
	if alive ||
		!errors.Is(err, context.Canceled) ||
		!errors.Is(err, closeCause) {
		t.Fatalf("Alive() = %t, %v, want false with context/close chains", alive, err)
	}
	if waitCalls != 0 || closeCalls != 1 {
		t.Fatalf("wait/close calls = %d/%d, want 0/1", waitCalls, closeCalls)
	}
}

func TestSystemPIDProbe_AccessDeniedIsUnknown(t *testing.T) {
	t.Parallel()

	api := completeFakeProcessAPI()
	api.openProcess = func(
		access uint32,
		inherit bool,
		pid uint32,
	) (windows.Handle, error) {
		if access != windows.SYNCHRONIZE || inherit || pid != 12345 {
			t.Fatalf(
				"OpenProcess access/inherit/pid = %#x/%t/%d",
				access,
				inherit,
				pid,
			)
		}
		return 0, windows.ERROR_ACCESS_DENIED
	}
	probe := newSystemPIDProbeWith(api)
	inspection := InspectTransaction(
		t.Context(),
		TransactionBackend,
		validTransactionState(TransactionBackend),
		probe,
		&fakeMutexProbe{},
	)
	if inspection.Activity != ActivityUnknown {
		t.Fatalf("Activity = %q, want unknown", inspection.Activity)
	}
	if !errors.Is(inspection.ProbeError, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf(
			"ProbeError = %v, want ERROR_ACCESS_DENIED",
			inspection.ProbeError,
		)
	}
	var probeErr *PIDProbeError
	if !errors.As(inspection.ProbeError, &probeErr) ||
		probeErr.Operation != "open-process" {
		t.Fatalf("ProbeError = %v, want open PIDProbeError", inspection.ProbeError)
	}
}

func TestSystemPIDProbe_PreservesWaitCloseAndContextFailures(t *testing.T) {
	t.Parallel()

	waitCause := errors.New("wait failed")
	closeCause := errors.New("close failed")
	ctx, cancel := context.WithCancel(t.Context())
	api := completeFakeProcessAPI()
	api.waitForSingleObject = func(
		windows.Handle,
		uint32,
	) (uint32, error) {
		cancel()
		return windows.WAIT_FAILED, waitCause
	}
	api.closeHandle = func(windows.Handle) error {
		return closeCause
	}

	alive, err := newSystemPIDProbeWith(api).Alive(ctx, 4242)
	if alive || err == nil {
		t.Fatalf("Alive() = %t, %v, want false/error", alive, err)
	}
	for _, want := range []error{waitCause, closeCause, context.Canceled} {
		if !errors.Is(err, want) {
			t.Fatalf("Alive() error = %v, want chain %v", err, want)
		}
	}
}

func TestSystemPIDProbe_WaitAndCloseFailuresAreUnknown(t *testing.T) {
	t.Parallel()

	waitCause := errors.New("wait failed")
	closeCause := errors.New("close failed")
	tests := []struct {
		name      string
		waitValue uint32
		waitErr   error
		closeErr  error
		want      []error
	}{
		{
			name:      "wait",
			waitValue: windows.WAIT_FAILED,
			waitErr:   waitCause,
			want:      []error{waitCause},
		},
		{
			name:      "unexpected_wait",
			waitValue: 0x42,
			want:      []error{errUnexpectedProcessWait},
		},
		{
			name:      "close",
			waitValue: uint32(windows.WAIT_TIMEOUT),
			closeErr:  closeCause,
			want:      []error{closeCause},
		},
		{
			name:      "wait_and_close",
			waitValue: windows.WAIT_FAILED,
			waitErr:   waitCause,
			closeErr:  closeCause,
			want:      []error{waitCause, closeCause},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			api := completeFakeProcessAPI()
			api.waitForSingleObject = func(
				windows.Handle,
				uint32,
			) (uint32, error) {
				return test.waitValue, test.waitErr
			}
			api.closeHandle = func(windows.Handle) error {
				return test.closeErr
			}
			probe := newSystemPIDProbeWith(api)
			alive, err := probe.Alive(t.Context(), 4242)
			if alive || err == nil {
				t.Fatalf("Alive() = %t, %v, want false/error", alive, err)
			}
			for _, want := range test.want {
				if !errors.Is(err, want) {
					t.Fatalf("Alive() error = %v, want chain %v", err, want)
				}
			}
		})
	}
}

func TestSystemPIDProbe_ChecksContextBeforeAndAfterWait(t *testing.T) {
	t.Parallel()

	t.Run("before", func(t *testing.T) {
		openCalls := 0
		api := completeFakeProcessAPI()
		api.openProcess = func(
			uint32,
			bool,
			uint32,
		) (windows.Handle, error) {
			openCalls++
			return fakeProcessHandle, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		alive, err := newSystemPIDProbeWith(api).Alive(ctx, 4242)
		if alive || !errors.Is(err, context.Canceled) {
			t.Fatalf("Alive(canceled) = %t, %v", alive, err)
		}
		if openCalls != 0 {
			t.Fatalf("OpenProcess calls = %d, want 0", openCalls)
		}
	})

	t.Run("after_wait", func(t *testing.T) {
		waitEntered := make(chan struct{})
		releaseWait := make(chan struct{})
		closeCalls := 0
		api := completeFakeProcessAPI()
		api.waitForSingleObject = func(
			windows.Handle,
			uint32,
		) (uint32, error) {
			close(waitEntered)
			<-releaseWait
			return uint32(windows.WAIT_TIMEOUT), nil
		}
		api.closeHandle = func(windows.Handle) error {
			closeCalls++
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, err := newSystemPIDProbeWith(api).Alive(ctx, 4242)
			result <- err
		}()
		waitForTestSignal(t, waitEntered, "process wait barrier")
		cancel()
		close(releaseWait)
		err := receiveTestValue(t, result, "PID probe result")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Alive() error = %v, want context.Canceled", err)
		}
		if closeCalls != 1 {
			t.Fatalf("CloseHandle calls = %d, want 1", closeCalls)
		}
	})
}

func TestSystemPIDProbe_RejectsZeroPID(t *testing.T) {
	t.Parallel()

	openCalls := 0
	api := completeFakeProcessAPI()
	api.openProcess = func(
		uint32,
		bool,
		uint32,
	) (windows.Handle, error) {
		openCalls++
		return fakeProcessHandle, nil
	}
	probe := newSystemPIDProbeWith(api)
	alive, err := probe.Alive(t.Context(), 0)
	if alive || !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("Alive(0) = %t, %v, want false/ErrInvalidPID", alive, err)
	}
	if openCalls != 0 {
		t.Fatalf("OpenProcess calls = %d, want 0", openCalls)
	}
}

func TestSystemPIDProbe_UsesOnlySynchronizeAndClosesHandle(t *testing.T) {
	t.Parallel()

	openCalls := 0
	closeCalls := 0
	api := completeFakeProcessAPI()
	api.openProcess = func(
		access uint32,
		inherit bool,
		pid uint32,
	) (windows.Handle, error) {
		openCalls++
		if access != windows.SYNCHRONIZE || inherit || pid != 4242 {
			t.Fatalf(
				"OpenProcess access/inherit/pid = %#x/%t/%d",
				access,
				inherit,
				pid,
			)
		}
		return fakeProcessHandle, nil
	}
	api.closeHandle = func(handle windows.Handle) error {
		closeCalls++
		if handle != fakeProcessHandle {
			t.Fatalf("CloseHandle handle = %#x, want %#x", handle, fakeProcessHandle)
		}
		return nil
	}
	alive, err := newSystemPIDProbeWith(api).Alive(t.Context(), 4242)
	if err != nil || !alive {
		t.Fatalf("Alive() = %t, %v, want true/nil", alive, err)
	}
	if openCalls != 1 || closeCalls != 1 {
		t.Fatalf("open/close calls = %d/%d, want 1/1", openCalls, closeCalls)
	}
}
