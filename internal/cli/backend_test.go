package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestBackendSupervise_RequiresExplicitManagedMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		modeArgs []string
		wantCode protocol.Code
	}{
		{name: "missing", wantCode: protocol.CodeInvalidArgument},
		{name: "development requires repo", modeArgs: []string{"--mode", "development"}, wantCode: protocol.CodeInvalidArgument},
		{name: "unknown", modeArgs: []string{"--mode", "future"}, wantCode: protocol.CodeUnsupportedMode},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var factoryCalls int
			var stdout, stderr bytes.Buffer
			args := []string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise"}
			args = append(args, test.modeArgs...)
			code := Execute(
				context.Background(),
				args,
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					factoryCalls++
					return backendServiceFunc(func(context.Context, backend.Request) error { return nil }), nil
				}),
			)
			if factoryCalls != 0 {
				t.Fatalf("backend factory calls = %d, want 0", factoryCalls)
			}
			definition, ok := protocol.LookupErrorDefinition(test.wantCode)
			if !ok {
				t.Fatalf("LookupErrorDefinition(%s) missing", test.wantCode)
			}
			if code != definition.ExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, definition.ExitCode, stderr.String())
			}
			events := parseNDJSON(t, stdout.String())
			if got := eventString(events[len(events)-1], "code"); got != string(test.wantCode) {
				t.Fatalf("result code = %q, want %q", got, test.wantCode)
			}
		})
	}
}

// TestBackendSupervise_ShutdownTimeoutArgument 覆盖增补 1 C9 的参数契约：
// 正整数秒、合法范围 1~120、默认 5；越界或非整数映射 INVALID_ARGUMENT 并
// 在建立任何后端资源之前失败关闭。
func TestBackendSupervise_ShutdownTimeoutArgument(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "default", want: 5 * time.Second},
		{name: "lower bound", args: []string{"--shutdown-timeout", "1"}, want: time.Second},
		{name: "upper bound", args: []string{"--shutdown-timeout", "120"}, want: 120 * time.Second},
		{name: "middle", args: []string{"--shutdown-timeout", "30"}, want: 30 * time.Second},
	}
	for _, test := range accepted {
		t.Run("accepted/"+test.name, func(t *testing.T) {
			t.Parallel()
			var captured backend.Request
			var stdout, stderr bytes.Buffer
			args := []string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"}
			args = append(args, test.args...)
			code := Execute(
				context.Background(),
				args,
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					return backendServiceFunc(func(_ context.Context, request backend.Request) error {
						captured = request
						return nil
					}), nil
				}),
			)
			if code != protocol.ExitCodeSuccess {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got := captured.ShutdownTimeout; got != test.want {
				t.Fatalf("shutdown timeout = %v, want %v", got, test.want)
			}
		})
	}

	rejected := []struct {
		name  string
		value string
	}{
		{name: "zero", value: "0"},
		{name: "above upper bound", value: "121"},
		{name: "negative", value: "-1"},
		{name: "not an integer", value: "abc"},
		{name: "fractional", value: "1.5"},
		{name: "empty", value: ""},
	}
	for _, test := range rejected {
		t.Run("rejected/"+test.name, func(t *testing.T) {
			t.Parallel()
			var factoryCalls int
			var stdout, stderr bytes.Buffer
			code := Execute(
				context.Background(),
				[]string{
					"--app-root", t.TempDir(), "--output", "ndjson",
					"backend", "supervise", "--mode", "managed", "--shutdown-timeout", test.value,
				},
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					factoryCalls++
					return backendServiceFunc(func(context.Context, backend.Request) error { return nil }), nil
				}),
			)
			if factoryCalls != 0 {
				t.Fatalf("backend factory calls = %d, want 0", factoryCalls)
			}
			definition, ok := protocol.LookupErrorDefinition(protocol.CodeInvalidArgument)
			if !ok {
				t.Fatal("INVALID_ARGUMENT definition is missing")
			}
			if code != definition.ExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, definition.ExitCode, stderr.String())
			}
			events := parseNDJSON(t, stdout.String())
			result := events[len(events)-1]
			if got := eventString(result, "code"); got != string(protocol.CodeInvalidArgument) {
				t.Fatalf("result code = %q, want INVALID_ARGUMENT", got)
			}
			details, ok := result.object["details"].(map[string]any)
			if !ok || details["field"] != "shutdown-timeout" {
				t.Fatalf("result details = %#v, want field=shutdown-timeout", result.object["details"])
			}
		})
	}
}

func TestBackendDevelopment_CLIResolvesExplicitRepoFromCWD(t *testing.T) {
	cwd := t.TempDir()
	var captured backend.Request
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "development", "--repo", "source"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(cwd),
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(_ context.Context, request backend.Request) error {
				captured = request
				return nil
			}), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if captured.Mode != backend.ModeDevelopment {
		t.Fatalf("backend mode = %q, want development", captured.Mode)
	}
	if got, want := captured.DevelopmentRepo, filepath.Join(cwd, "source"); got != want {
		t.Fatalf("development repo = %q, want %q", got, want)
	}
}

// TestBackendSupervise_PassesMirrorPolicyToFactory 锁定增补 1 C11 的数据流起点：
// 全局镜像策略只在 CLI 侧被解析，backend 需要它才能算出下发给后端的有序源列表。
func TestBackendSupervise_PassesMirrorPolicyToFactory(t *testing.T) {
	tests := []struct {
		name          string
		arguments     []string
		wantOffline   bool
		wantOnly      bool
		wantPreferred string
	}{
		{name: "default policy", arguments: nil},
		{name: "offline", arguments: []string{"--offline"}, wantOffline: true},
		{name: "mirror only", arguments: []string{"--mirror-only"}, wantOnly: true},
		{
			name:          "explicit package index preference",
			arguments:     []string{"--mirror", "package-index=ustc"},
			wantPreferred: "ustc",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var captured mirror.Policy
			var stdout, stderr bytes.Buffer
			arguments := append([]string{"--app-root", t.TempDir(), "--output", "ndjson"}, test.arguments...)
			arguments = append(arguments, "backend", "supervise", "--mode", "managed")
			code := Execute(
				context.Background(),
				arguments,
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(_ context.Context, _ *config.Layout, _ io.Writer, _ func() time.Time, policy mirror.Policy) (backendService, error) {
					captured = policy
					return backendServiceFunc(func(context.Context, backend.Request) error { return nil }), nil
				}),
			)
			if code != protocol.ExitCodeSuccess {
				t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got := captured.Offline(); got != test.wantOffline {
				t.Errorf("policy offline = %t, want %t", got, test.wantOffline)
			}
			if got := captured.MirrorOnly(); got != test.wantOnly {
				t.Errorf("policy mirrorOnly = %t, want %t", got, test.wantOnly)
			}
			preferred, _ := captured.Preferred(mirror.KindPackageIndex)
			if preferred != test.wantPreferred {
				t.Errorf("preferred package index = %q, want %q", preferred, test.wantPreferred)
			}
		})
	}
}

type backendServiceFunc func(context.Context, backend.Request) error

func (f backendServiceFunc) Supervise(ctx context.Context, request backend.Request) error {
	return f(ctx, request)
}

func TestBackend_ControlReaderJoinAndReadFailure(t *testing.T) {
	// 增补 1 C13：stdin 读取出错等价于宿主断开，走隐式 shutdown 而不是 INTERNAL_ERROR，
	// 但 Execute 仍必须在有限时间内收口。
	t.Run("reader failure joins within bound and shuts down", func(t *testing.T) {
		input := &backendReadError{err: errors.New("stdin read failed")}
		started := make(chan struct{})
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- Execute(
				context.Background(),
				[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
				IO{In: input, Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
						close(started)
						command, err := request.Control.Receive(ctx)
						if err != nil {
							return err
						}
						if command.Command != protocol.ControlShutdown {
							return errors.New("unexpected control command")
						}
						request.BeforeShutdown(command.CommandID)
						return nil
					}), nil
				}),
			)
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("backend service did not start")
		}
		select {
		case code := <-done:
			if code != protocol.ExitCodeSuccess {
				t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
		case <-time.After(time.Second):
			t.Fatal("Execute() did not join failed control reader")
		}
		events := parseNDJSON(t, stdout.String())
		if got := eventString(events[len(events)-1], "status"); got != string(protocol.StateStopped) {
			t.Fatalf("result status = %q, want stopped", got)
		}
		assertBackendCapabilities(t, events[0])
	})

	t.Run("shutdown keeps command id", func(t *testing.T) {
		const commandID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		input := strings.NewReader(`{"protocol":1,"command":"shutdown","commandId":"` + commandID + `"}` + "\n")
		var stdout, stderr bytes.Buffer
		code := Execute(
			context.Background(),
			[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
			IO{In: input, Out: &stdout, Err: &stderr},
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
				return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
					command, err := request.Control.Receive(ctx)
					if err != nil {
						return err
					}
					if command.Command != protocol.ControlShutdown {
						return errors.New("unexpected control command")
					}
					request.BeforeShutdown(command.CommandID)
					return nil
				}), nil
			}),
		)
		if code != protocol.ExitCodeSuccess {
			t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
		}
		events := parseNDJSON(t, stdout.String())
		assertBackendCapabilities(t, events[0])
		result := events[len(events)-1]
		if got := eventString(result, "status"); got != string(protocol.StateStopped) {
			t.Fatalf("result status = %q, want stopped", got)
		}
		details, ok := result.object["details"].(map[string]any)
		if !ok || details["controlCommandId"] != commandID {
			t.Fatalf("result details = %#v, want controlCommandId=%q", result.object["details"], commandID)
		}
	})

	t.Run("cancel first keeps shutdown and status in FIFO", func(t *testing.T) {
		cancelID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		shutdownID := "01ARZ3NDEKTSV4RRFFQ69G5FAW"
		statusID := "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		input := strings.NewReader(
			`{"protocol":1,"command":"cancel","commandId":"` + cancelID + `"}` + "\n" +
				`{"protocol":1,"command":"shutdown","commandId":"` + shutdownID + `"}` + "\n" +
				`{"protocol":1,"command":"status","commandId":"` + statusID + `"}` + "\n",
		)
		var stdout, stderr bytes.Buffer
		var got []protocol.ControlCommand
		code := Execute(
			context.Background(),
			[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
			IO{In: input, Out: &stdout, Err: &stderr},
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
				return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
					for range 3 {
						command, err := request.Control.Receive(ctx)
						if err != nil {
							return err
						}
						got = append(got, command)
					}
					return nil
				}), nil
			}),
		)
		if code != protocol.ExitCodeSuccess {
			t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
		}
		if len(got) != 3 || got[0].CommandID != cancelID || got[1].CommandID != shutdownID || got[2].CommandID != statusID {
			t.Fatalf("received commands = %#v, want cancel/shutdown/status FIFO", got)
		}
	})

	t.Run("external cancel joins bounded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- Execute(
				ctx,
				[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					return backendServiceFunc(func(ctx context.Context, _ backend.Request) error {
						close(started)
						<-ctx.Done()
						return ctx.Err()
					}), nil
				}),
			)
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("backend service did not start")
		}
		cancel()
		select {
		case code := <-done:
			if code != protocol.ExitCodeOperationCancelled {
				t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", code, protocol.ExitCodeOperationCancelled, stderr.String())
			}
		case <-time.After(time.Second):
			t.Fatal("Execute() did not join external cancellation")
		}
	})
}

func assertBackendCapabilities(t *testing.T, hello parsedEvent) {
	t.Helper()
	capabilities, ok := hello.object["capabilities"].([]any)
	if !ok {
		t.Fatalf("hello capabilities = %#v, want array", hello.object["capabilities"])
	}
	wanted := map[string]bool{
		string(protocol.CapabilityStdinCancel): false,
		string(protocol.CapabilityStateV1):     false,
		string(protocol.CapabilityLogStream):   false,
	}
	for _, value := range capabilities {
		if name, ok := value.(string); ok {
			if _, exists := wanted[name]; exists {
				wanted[name] = true
			}
		}
	}
	for name, found := range wanted {
		if !found {
			t.Errorf("hello capabilities missing %q", name)
		}
	}
}

type backendReadError struct {
	once sync.Once
	err  error
}

func (r *backendReadError) Read([]byte) (int, error) {
	r.once.Do(func() {})
	return 0, r.err
}

func (*backendReadError) Close() error { return nil }

// TestBackendSupervise_PortArgument 覆盖增补 1 C12 的参数契约：整数、合法范围
// 1024~65535，缺省按模式 managed 36163 / development 36164；越界或非整数映射
// INVALID_ARGUMENT 并在建立任何后端资源之前失败关闭。
func TestBackendSupervise_PortArgument(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		name string
		mode []string
		args []string
		want int
	}{
		{name: "managed default", mode: []string{"--mode", "managed"}, want: 36163},
		{name: "development default", mode: []string{"--mode", "development", "--repo", "source"}, want: 36164},
		{name: "lower bound", mode: []string{"--mode", "managed"}, args: []string{"--port", "1024"}, want: 1024},
		{name: "upper bound", mode: []string{"--mode", "managed"}, args: []string{"--port", "65535"}, want: 65535},
		{name: "development explicit", mode: []string{"--mode", "development", "--repo", "source"}, args: []string{"--port", "36170"}, want: 36170},
	}
	for _, test := range accepted {
		t.Run("accepted/"+test.name, func(t *testing.T) {
			t.Parallel()
			var captured backend.Request
			var stdout, stderr bytes.Buffer
			args := []string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise"}
			args = append(args, test.mode...)
			args = append(args, test.args...)
			code := Execute(
				context.Background(),
				args,
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithCWD(t.TempDir()),
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					return backendServiceFunc(func(_ context.Context, request backend.Request) error {
						captured = request
						return nil
					}), nil
				}),
			)
			if code != protocol.ExitCodeSuccess {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got := captured.Port; got != test.want {
				t.Fatalf("supervised port = %d, want %d", got, test.want)
			}
		})
	}

	rejected := []struct {
		name  string
		value string
	}{
		{name: "below lower bound", value: "1023"},
		{name: "above upper bound", value: "65536"},
		{name: "zero", value: "0"},
		{name: "negative", value: "-1"},
		{name: "not an integer", value: "abc"},
		{name: "fractional", value: "1.5"},
		{name: "empty", value: ""},
	}
	for _, test := range rejected {
		t.Run("rejected/"+test.name, func(t *testing.T) {
			t.Parallel()
			var factoryCalls int
			var stdout, stderr bytes.Buffer
			code := Execute(
				context.Background(),
				[]string{
					"--app-root", t.TempDir(), "--output", "ndjson",
					"backend", "supervise", "--mode", "managed", "--port", test.value,
				},
				IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
					factoryCalls++
					return backendServiceFunc(func(context.Context, backend.Request) error { return nil }), nil
				}),
			)
			if factoryCalls != 0 {
				t.Fatalf("backend factory calls = %d, want 0", factoryCalls)
			}
			definition, ok := protocol.LookupErrorDefinition(protocol.CodeInvalidArgument)
			if !ok {
				t.Fatal("INVALID_ARGUMENT definition is missing")
			}
			if code != definition.ExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, definition.ExitCode, stderr.String())
			}
			events := parseNDJSON(t, stdout.String())
			result := events[len(events)-1]
			if got := eventString(result, "code"); got != string(protocol.CodeInvalidArgument) {
				t.Fatalf("result code = %q, want INVALID_ARGUMENT", got)
			}
			details, ok := result.object["details"].(map[string]any)
			if !ok || details["field"] != "port" {
				t.Fatalf("result details = %#v, want field=port", result.object["details"])
			}
		})
	}
}

// implicitShutdownService 是 C13 用例共用的假监督器：只接收一条控制命令并把它交给断言，
// 收到 shutdown 时像真实监督器一样调用 BeforeShutdown 后正常返回。
func implicitShutdownService(received chan<- protocol.ControlCommand) backendService {
	return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		command, err := request.Control.Receive(waitCtx)
		if err != nil {
			return err
		}
		received <- command
		if command.Command != protocol.ControlShutdown {
			return errors.New("unexpected control command")
		}
		request.BeforeShutdown(command.CommandID)
		return nil
	})
}

// TestBackendSupervise_StdinEOFSubmitsImplicitShutdown 锁定增补 1 C13 的主路径：
// hello 之后 stdin 到达 EOF，监督器从同一个 mailbox 收到一条没有 commandId 的 shutdown，
// 结局与显式 shutdown 相同（result.status=stopped、退出码 0），只是不回显 controlCommandId。
func TestBackendSupervise_StdinEOFSubmitsImplicitShutdown(t *testing.T) {
	received := make(chan protocol.ControlCommand, 1)
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return implicitShutdownService(received), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	select {
	case command := <-received:
		if command.Command != protocol.ControlShutdown || command.CommandID != "" {
			t.Fatalf("received command = %#v, want implicit shutdown without commandId", command)
		}
	default:
		t.Fatal("backend service received no control command")
	}
	events := parseNDJSON(t, stdout.String())
	result := events[len(events)-1]
	if got := eventString(result, "status"); got != string(protocol.StateStopped) {
		t.Fatalf("result status = %q, want stopped", got)
	}
	if details, ok := result.object["details"].(map[string]any); ok {
		if _, exists := details["controlCommandId"]; exists {
			t.Fatalf("result details = %#v, want no controlCommandId for implicit shutdown", details)
		}
	}
}

// TestBackendSupervise_StdinReadErrorSubmitsImplicitShutdown 证明读取出错与 EOF 同样
// 视为宿主断开：监督器收到隐式 shutdown、退出码 0，且 stderr 保留一条诊断。
func TestBackendSupervise_StdinReadErrorSubmitsImplicitShutdown(t *testing.T) {
	received := make(chan protocol.ControlCommand, 1)
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: &backendReadError{err: errors.New("stdin read failed")}, Out: &stdout, Err: &stderr},
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return implicitShutdownService(received), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	select {
	case command := <-received:
		if command.Command != protocol.ControlShutdown || command.CommandID != "" {
			t.Fatalf("received command = %#v, want implicit shutdown without commandId", command)
		}
	default:
		t.Fatal("backend service received no control command")
	}
	if !strings.Contains(stderr.String(), "stdin read failed") {
		t.Fatalf("stderr = %q, want a diagnostic naming the read failure", stderr.String())
	}
	events := parseNDJSON(t, stdout.String())
	if got := eventString(events[len(events)-1], "status"); got != string(protocol.StateStopped) {
		t.Fatalf("result status = %q, want stopped", got)
	}
}

// TestBackendSupervise_ExplicitShutdownThenEOFIsIdempotent 证明显式 shutdown 之后
// 再到达的 EOF 不产生第二次关闭：监督器只收到那一条带 commandId 的 shutdown，
// 后续 Receive 立即得到「已停止」，result 回显的是显式那条的 commandId。
func TestBackendSupervise_ExplicitShutdownThenEOFIsIdempotent(t *testing.T) {
	const commandID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var stdout, stderr bytes.Buffer
	var got []protocol.ControlCommand
	var secondErr error
	code := Execute(
		context.Background(),
		[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
		IO{In: strings.NewReader(`{"protocol":1,"command":"shutdown","commandId":"` + commandID + `"}` + "\n"), Out: &stdout, Err: &stderr},
		WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time, mirror.Policy) (backendService, error) {
			return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
				command, err := request.Control.Receive(ctx)
				if err != nil {
					return err
				}
				got = append(got, command)
				waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
				second, err := request.Control.Receive(waitCtx)
				if err == nil {
					got = append(got, second)
				}
				secondErr = err
				request.BeforeShutdown(command.CommandID)
				return nil
			}), nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if len(got) != 1 || got[0].Command != protocol.ControlShutdown || got[0].CommandID != commandID {
		t.Fatalf("received commands = %#v, want exactly the explicit shutdown", got)
	}
	if !errors.Is(secondErr, backend.ErrControlStopped) {
		t.Fatalf("second Receive error = %v, want ErrControlStopped (no implicit shutdown after explicit one)", secondErr)
	}
	events := parseNDJSON(t, stdout.String())
	details, ok := events[len(events)-1].object["details"].(map[string]any)
	if !ok || details["controlCommandId"] != commandID {
		t.Fatalf("result details = %#v, want controlCommandId=%q", events[len(events)-1].object["details"], commandID)
	}
}
