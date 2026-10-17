//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestExecute_PlatformBoundary(t *testing.T) {
	t.Setenv("AUTO_MAS_TELEMETRY", "enabled")
	for _, command := range [][]string{{"version"}, {"doctor"}, {"bootstrap"}, {"workspace", "check"}, {"workspace", "check", "--remote"}, {"workspace", "sync", "--version", "v1.0.0"}, {"workspace", "stage", "--version", "v1.0.0"}, {"environment", "check"}, {"environment", "ensure"}, {"environment", "repair"}, {"dependencies", "sync"}, {"dependencies", "check"}, {"dependencies", "rebuild"}, {"backend", "supervise", "--mode", "managed"}, {"repair"}, {"cleanup"}} {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "尚未创建")
			var stdout, stderr bytes.Buffer
			args := append([]string{"--output", "ndjson", "--app-root", root}, command...)
			code := Execute(t.Context(), args, IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
				WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
					t.Fatal("workspace factory called before native validation")
					return nil, nil
				}),
				WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
					t.Fatal("environment factory called before native validation")
					return nil, nil
				}),
				WithTelemetryFactory(func(telemetry.Config) telemetry.Recorder {
					t.Fatal("telemetry factory called before native validation")
					return nil
				}),
			)
			wantCode := protocol.ExitCodeInvalidArgument
			if command[0] == "version" {
				wantCode = 0
			}
			if code != wantCode {
				t.Fatalf("got exit %d, want %d; stdout=%s stderr=%s", code, wantCode, stdout.String(), stderr.String())
			}
			lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
			var events []map[string]any
			for i, line := range lines {
				var event map[string]any
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatalf("line %d: %v", i, err)
				}
				if event["sequence"] != float64(i+1) {
					t.Fatalf("got sequence %v, want %d", event["sequence"], i+1)
				}
				events = append(events, event)
			}
			if events[0]["type"] != "hello" || events[len(events)-1]["type"] != "result" {
				t.Fatalf("got events %v, want hello through result", events)
			}
			if command[0] != "version" {
				if len(events) != 3 || events[1]["code"] != string(protocol.CodeUnsupportedMode) || events[2]["code"] != string(protocol.CodeUnsupportedMode) {
					t.Fatalf("got %v, want hello/error/result with UNSUPPORTED_MODE", events)
				}
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatalf("got root stat %v, want no directory created", err)
			}
		})
	}
}
