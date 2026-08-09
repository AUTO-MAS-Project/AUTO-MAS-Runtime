package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
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
		{name: "development deferred", modeArgs: []string{"--mode", "development"}, wantCode: protocol.CodeUnsupportedMode},
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
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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

type backendServiceFunc func(context.Context, backend.Request) error

func (f backendServiceFunc) Supervise(ctx context.Context, request backend.Request) error {
	return f(ctx, request)
}

func TestBackend_ControlReaderJoinAndReadFailure(t *testing.T) {
	t.Run("reader failure joins within bound", func(t *testing.T) {
		input := &backendReadError{err: errors.New("stdin read failed")}
		started := make(chan struct{})
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- Execute(
				context.Background(),
				[]string{"--app-root", t.TempDir(), "--output", "ndjson", "backend", "supervise", "--mode", "managed"},
				IO{In: input, Out: &stdout, Err: &stderr},
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
					return backendServiceFunc(func(ctx context.Context, request backend.Request) error {
						close(started)
						_, err := request.Control.Receive(ctx)
						return err
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
			if code != protocol.ExitCodePreconditionFailed {
				t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", code, protocol.ExitCodePreconditionFailed, stderr.String())
			}
		case <-time.After(time.Second):
			t.Fatal("Execute() did not join failed control reader")
		}
		events := parseNDJSON(t, stdout.String())
		if got := eventString(events[len(events)-1], "code"); got != string(protocol.CodeInternalError) {
			t.Fatalf("result code = %q, want INTERNAL_ERROR", got)
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
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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
			WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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
				WithBackendFactory(func(context.Context, *config.Layout, io.Writer, func() time.Time) (backendService, error) {
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
