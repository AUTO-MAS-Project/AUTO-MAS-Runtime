package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
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
