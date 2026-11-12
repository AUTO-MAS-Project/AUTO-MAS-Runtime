package uv

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type failingIsolationJob struct {
	assigned uint32
	closed   bool
}

func (j *failingIsolationJob) Assign(pid uint32) error { j.assigned = pid; return syscall.Errno(5) }
func (j *failingIsolationJob) Terminate(uint32) error  { return nil }
func (j *failingIsolationJob) Close() error            { j.closed = true; return nil }

func TestRunner_IsolationFailureDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		assign bool
	}{{name: "create_job"}, {name: "assign_job", assign: true}} {
		t.Run(test.name, func(t *testing.T) {
			runner := newTestRunner(t)
			job := &failingIsolationJob{}
			runner.newJob = func() (runnerJob, error) {
				if test.assign {
					return job, nil
				}
				return nil, syscall.Errno(5)
			}
			_, err := runner.Run(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, RunOptions{Stage: protocol.StageUVVerify})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code() != protocol.CodeUVExecFailed {
				t.Fatalf("error = %v, want UV_EXEC_FAILED", err)
			}
			details := failure.DiagnosticDetails()
			if details["operation"] != test.name || details["windowsError"] != uint64(5) || details["durationMs"] == nil {
				t.Fatalf("diagnostics = %#v, want operation, errno and elapsed time", details)
			}
			if len(failure.Details()) != 1 || failure.Details()["failureKind"] != "process_isolation_failed" {
				t.Fatalf("protocol details changed: %#v", failure.Details())
			}
			if test.assign && (job.assigned == 0 || !job.closed) {
				t.Fatalf("job = %+v, want assign and close", job)
			}
		})
	}
}

func TestPython_InstallFailureDiagnostics(t *testing.T) {
	for _, name := range []string{"offline", "relay", "rotation"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, root)
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			writePythonProject(t, project, "3.12.10", "[project]\nrequires-python = \">=3.12\"\n")
			last := newError(protocol.CodeUVExecFailed, protocol.StagePythonInstall, "读取失败", map[string]any{"failureKind": "output_read_failed", "operation": "read", "windowsError": uint64(6)}, errors.New("private cause"))
			response := fakeRunnerResponse{result: UVResult{ExitCode: 0, Stderr: "private stderr", Duration: 125 * time.Millisecond}, err: last}
			runner := &fakePythonRunner{listOutput: `[{"version":"3.12.10"}]`, installResults: []fakeRunnerResponse{response}}
			service, err := NewPythonService(layout, runner)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := mirror.NewPolicy(mirror.PolicySpec{Offline: name == "offline", MirrorOnly: name != "offline"})
			if err != nil {
				t.Fatal(err)
			}
			wantAttempts := 1
			if name == "relay" {
				factory := &fakeRelayFactory{session: &fakeRelaySession{baseURL: "http://127.0.0.1:39171"}}
				service.relay = factory.start
			}
			if name == "rotation" {
				service.network.rotator, err = mirror.NewRotator(mirror.WithMaxSourceAttempts(2), mirror.WithRotatorWait(func(context.Context, time.Duration) error { return nil }))
				if err != nil {
					t.Fatal(err)
				}
				plan, planErr := service.network.buildPlan(t.Context(), policy, mirror.KindPython)
				if planErr != nil {
					t.Fatal(planErr)
				}
				wantAttempts = 2 * len(plan.Sources())
				runner.installResults = make([]fakeRunnerResponse, wantAttempts)
				for i := range runner.installResults {
					runner.installResults[i] = fakeRunnerResponse{result: UVResult{ExitCode: 2}, err: newError(protocol.CodeUVExecFailed, protocol.StagePythonInstall, "退出失败", map[string]any{"failureKind": "nonzero_exit"}, errors.New("earlier failure"))}
				}
				runner.installResults[wantAttempts-1] = response
			}
			_, err = service.Prepare(t.Context(), PythonRequest{ProjectDir: project, MirrorPolicy: policy})
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want Python failure", err)
			}
			details := failure.DiagnosticDetails()
			if details["failureKind"] != "output_read_failed" || details["exitCode"] != 0 || details["windowsError"] != uint64(6) || details["durationMs"] != int64(125) {
				t.Fatalf("diagnostics = %#v, want last execution facts", details)
			}
			if name != "relay" && details["attemptCount"] != wantAttempts {
				t.Fatalf("attempt count = %#v, want %d", details["attemptCount"], wantAttempts)
			}
			if name == "rotation" {
				attempts := details["attempts"].([]map[string]any)
				if len(attempts) > maxNetworkDiagnosticAttempts || attempts[len(attempts)-1]["globalTry"] != wantAttempts {
					t.Fatalf("attempts = %#v, want bounded tail including last attempt", attempts)
				}
			}
		})
	}
}
