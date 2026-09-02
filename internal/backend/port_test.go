package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// TestBackend_PortDefaultsByModeAndDerivesAddresses 锁定增补 1 C12 的三件事在
// managed（无控制路径）与 development（受控路径）下同时成立：缺省端口按模式给出、
// 显式端口原样使用；注入 uv 的 Port、健康检查的 Expectation.Port 与 running 事件的
// baseUrl 三者由同一个端口派生。
func TestBackend_PortDefaultsByModeAndDerivesAddresses(t *testing.T) {
	tests := []struct {
		name        string
		development bool
		port        int
		want        int
	}{
		{name: "managed default", want: health.DefaultPort},
		{name: "development default", development: true, want: 36164},
		{name: "managed explicit", port: 5555, want: 5555},
		{name: "development explicit", development: true, port: 6666, want: 6666},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newBackendFixture(t)
			f.proc.keepAlive = true
			request := f.request()
			request.Port = test.port
			if test.development {
				request = developmentRequest(request, newDevelopmentRepo(t))
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- f.supervisor().Supervise(ctx, request) }()
			waitFor(t, f.emitter.running)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) && !hasBackendCode(err, protocol.CodeOperationCancelled) {
				t.Fatalf("Supervise() error = %v, want cancellation", err)
			}
			if got := f.uv.options.Port; got != test.want {
				t.Errorf("ManagedOptions.Port = %d, want %d", got, test.want)
			}
			if len(f.health.expectations) == 0 {
				t.Fatal("health expectations are empty")
			}
			if got := f.health.expectations[0].Port; got != test.want {
				t.Errorf("health Expectation.Port = %d, want %d", got, test.want)
			}
			var running *protocol.StateEvent
			for _, event := range f.emitter.states() {
				if event.Status == protocol.StateRunning {
					clone := event
					running = &clone
					break
				}
			}
			if running == nil {
				t.Fatalf("states = %#v, want running", f.emitter.states())
			}
			if got, want := running.Details["baseUrl"], health.BaseURL(test.want); got != want {
				t.Errorf("running baseUrl = %#v, want %q", got, want)
			}
		})
	}
}

// TestBackend_RestartReusesSupervisedPort 证明单次自动重启的第二代进程拿到同一个端口。
func TestBackend_RestartReusesSupervisedPort(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	second := &fakeProcess{pid: 4343, keepAlive: true}
	f.uv.procSequence = []ManagedProcess{f.proc, second}
	var mu sync.Mutex
	var ports []int
	f.uv.onStart = func() {
		mu.Lock()
		ports = append(ports, f.uv.options.Port)
		mu.Unlock()
	}
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		req := f.request()
		req.Control = mailbox
		req.Port = 7777
		done <- f.supervisor().Supervise(t.Context(), req)
	}()
	waitFor(t, f.emitter.running)
	f.proc.Exit()
	waitForStateStatusCount(t, f.emitter, protocol.StateRunning, 2)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlCancel, CommandID: "cancel-after-restart"}); err != nil {
		t.Fatalf("Submit(cancel) error = %v", err)
	}
	if err := <-done; !hasBackendCode(err, protocol.CodeOperationCancelled) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want cancellation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ports) != 2 || ports[0] != 7777 || ports[1] != 7777 {
		t.Fatalf("supervised ports across restart = %v, want [7777 7777]", ports)
	}
	runningCount := 0
	for _, event := range f.emitter.states() {
		if event.Status != protocol.StateRunning {
			continue
		}
		runningCount++
		if got, want := event.Details["baseUrl"], health.BaseURL(7777); got != want {
			t.Errorf("running #%d baseUrl = %#v, want %q", runningCount, got, want)
		}
	}
}

// TestBackend_CloseRequestTargetsSupervisedPort 用真实回环 HTTP 服务证明关闭请求
// 打到派生端口的 /api/core/close，且非 2xx 仍按失败处理。
func TestBackend_CloseRequestTargetsSupervisedPort(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	status := http.StatusOK
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen = append(seen, request.Method+" "+request.URL.Path)
		code := status
		mu.Unlock()
		writer.WriteHeader(code)
	}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	closer := newLoopbackHTTPCloser(port)
	if err := closer.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	status = http.StatusServiceUnavailable
	mu.Unlock()
	if len(got) != 1 || got[0] != "POST /api/core/close" {
		t.Fatalf("requests = %#v, want a single POST /api/core/close on port %d", got, port)
	}
	if err := closer.Close(t.Context()); err == nil {
		t.Fatal("Close() error = nil, want failure for status 503")
	}
	if got, want := backendCloseURL(port), health.BaseURL(port)+"/api/core/close"; got != want {
		t.Fatalf("backendCloseURL(%d) = %q, want %q", port, got, want)
	}
}

// TestBackend_RejectsOutOfRangePort 证明越界端口在获取任何资源、创建任何进程之前
// 映射 INVALID_ARGUMENT。
func TestBackend_RejectsOutOfRangePort(t *testing.T) {
	for _, port := range []int{1023, 65536, -1} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			f := newBackendFixture(t)
			request := f.request()
			request.Port = port
			err := f.supervisor().Supervise(t.Context(), request)
			assertBackendCode(t, err, protocol.CodeInvalidArgument)
			if f.uv.startCalls != 0 {
				t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
			}
			if f.logger.closeCalls != 0 {
				t.Fatalf("logger close calls = %d, want 0 (no resources may be opened)", f.logger.closeCalls)
			}
		})
	}
}
