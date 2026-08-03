package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// fakeDoctorService 是契约测试使用的 doctor 服务替身。
type fakeDoctorService struct {
	run func(context.Context, *protocol.Emitter) (doctor.Report, error)
}

func (f fakeDoctorService) Run(ctx context.Context, emitter *protocol.Emitter) (doctor.Report, error) {
	return f.run(ctx, emitter)
}

func writeDoctorFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func healthyDoctorFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	versionData, err := json.Marshal(map[string]string{"version": "v5.4.0-beta.1"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	writeDoctorFixtureFile(t, layout.RepoVersionFile(), versionData)
	writeDoctorFixtureFile(t, filepath.Join(layout.PythonDir(), "python.exe"), nil)
	if err := os.MkdirAll(layout.VenvDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(venv) error = %v", err)
	}
	uvDir, err := layout.UVVersionDir("0.8.0")
	if err != nil {
		t.Fatalf("UVVersionDir() error = %v", err)
	}
	writeDoctorFixtureFile(t, filepath.Join(uvDir, "uv.exe"), nil)
	if err := os.MkdirAll(layout.LogsDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(logs) error = %v", err)
	}
	for _, file := range []string{
		layout.BackendStateFile(),
		layout.MutationStateFile(),
		layout.UpdateStateFile(),
		layout.EnvironmentStateFile(),
	} {
		writeDoctorFixtureFile(t, file, []byte("{}"))
	}
	return root
}

func doctorTestProbes() doctor.Probes {
	return doctor.Probes{
		UVVersion: func(context.Context, string) (string, error) {
			return "uv 0.8.0", nil
		},
		DiskFree: func(context.Context, string) (uint64, error) {
			return 1 << 40, nil
		},
	}
}

func TestDoctor_ServiceFailureMapsToCodedError(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{
				run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
					return doctor.Report{}, doctor.NewError(
						protocol.CodeMutexOperationFailed,
						protocol.StageDoctor,
						"锁探测失败",
						map[string]any{},
						nil,
					)
				},
			}, nil
		}),
	)
	if code != protocol.ExitCodeOperationConflict {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodeOperationConflict)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent, resultEvent parsedEvent
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeMutexOperationFailed) {
		t.Errorf("error code = %q, want MUTEX_OPERATION_FAILED", got)
	}
	if got := eventString(resultEvent, "code"); got != string(protocol.CodeMutexOperationFailed) {
		t.Errorf("result code = %q, want MUTEX_OPERATION_FAILED", got)
	}
}

func TestDoctor_ServiceCancelledMapsToOperationCancelled(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{
				run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
					return doctor.Report{}, context.Canceled
				},
			}, nil
		}),
	)
	if code != 130 {
		t.Fatalf("exit code = %d, want 130", code)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent, resultEvent parsedEvent
	for _, event := range events {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeOperationCancelled) {
		t.Errorf("error code = %q, want OPERATION_CANCELLED", got)
	}
	if got := eventString(resultEvent, "status"); got != "cancelled" {
		t.Errorf("result status = %q, want cancelled", got)
	}
}

func TestDoctorCommand_NDJSONReport(t *testing.T) {
	t.Parallel()
	root := healthyDoctorFixture(t)
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "--app-root", root, "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
		WithDoctorFactory(func(layout *config.Layout, _ doctor.Probes) (doctorService, error) {
			return doctor.New(layout, doctorTestProbes())
		}),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	events := parseNDJSON(t, stdout.String())
	last := events[len(events)-1]
	if eventType(last) != string(protocol.TypeResult) {
		t.Fatalf("last event type = %q, want result", eventType(last))
	}
	if success, ok := last.object["success"].(bool); !ok || !success {
		t.Errorf("result success = %v, want true", last.object["success"])
	}
	details, ok := last.object["details"].(map[string]any)
	if !ok {
		t.Fatalf("result details = %#v, want object", last.object["details"])
	}
	checks, ok := details["checks"].([]any)
	if !ok || len(checks) != 9 {
		t.Fatalf("details.checks = %#v, want 9 items", details["checks"])
	}
	summary, ok := details["summary"].(map[string]any)
	if !ok {
		t.Fatalf("details.summary = %#v, want object", details["summary"])
	}
	if summary["total"] != float64(9) || summary["ok"] != float64(9) {
		t.Errorf("summary = %#v, want 9/9 ok", summary)
	}
}

func TestDoctorCommand_HumanOutput(t *testing.T) {
	t.Parallel()
	root := healthyDoctorFixture(t)
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithDoctorFactory(func(layout *config.Layout, _ doctor.Probes) (doctorService, error) {
			return doctor.New(layout, doctorTestProbes())
		}),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "HELLO") ||
		!strings.Contains(stdout.String(), "PROGRESS") ||
		!strings.Contains(stdout.String(), "RESULT success=true") {
		t.Errorf("stdout = %q, want human hello/progress/result", stdout.String())
	}
}

// TestDoctor_FactorySetupErrorFallsBackToStableCode 证明服务构造失败这类
// 无法映射到业务错误码的内部故障走 INTERNAL_ERROR 兜底，而不是
// OUTPUT_WRITE_FAILED——后者的语义是协议输出通道写失败，用它兜底会把
// Runtime 自身的缺陷伪装成输出故障（T3.8 F13d）。
func TestDoctor_FactorySetupErrorFallsBackToStableCode(t *testing.T) {
	t.Parallel()
	var stdout strings.Builder
	var stderr strings.Builder
	code := Execute(
		context.Background(),
		[]string{"--output", "ndjson", "doctor"},
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		WithCWD(t.TempDir()),
		WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return nil, errors.New("factory failed")
		}),
	)
	if code != protocol.ExitCodePreconditionFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitCodePreconditionFailed)
	}
	events := parseNDJSON(t, stdout.String())
	var errorEvent parsedEvent
	for _, event := range events {
		if eventType(event) == string(protocol.TypeError) {
			errorEvent = event
		}
	}
	if got := eventString(errorEvent, "code"); got != string(protocol.CodeInternalError) {
		t.Errorf("error code = %q, want INTERNAL_ERROR", got)
	}
}
