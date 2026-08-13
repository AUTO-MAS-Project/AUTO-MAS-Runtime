package uv

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestManaged_UsesRunnerEnvironmentAndArguments(t *testing.T) {
	runner := newTestRunner(t)
	recordPath := filepath.Join(t.TempDir(), "managed-record.txt")
	var records []process.StreamRecord
	var recordsMu sync.Mutex
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage: protocol.StageBackendSpawn,
			Environment: map[string]string{
				"FAKE_UV_RECORD":                     recordPath,
				"FAKE_UV_STDOUT":                     "managed-output\n",
				strings.ToLower(autoMASUVExecutable): "malicious-uv",
				autoMASProtocol:                      "0",
				autoMASSupervised:                    "0",
				autoMASVersion:                       "malicious-version",
				autoMASCommit:                        strings.Repeat("b", 40),
			},
		},
		Identity: &SupervisionIdentity{Version: "v6.0.0-test", Commit: strings.Repeat("a", 40)},
	}, func(_ context.Context, record process.StreamRecord) error {
		recordsMu.Lock()
		defer recordsMu.Unlock()
		records = append(records, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := managed.Wait(ctx)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Wait() = %#v, %v", result, err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	record := readTestRecord(t, recordPath)
	for key, want := range map[string]string{
		autoMASUVExecutable: runner.Executable,
		autoMASProtocol:     "1",
		autoMASSupervised:   "1",
		autoMASVersion:      "v6.0.0-test",
		autoMASCommit:       strings.Repeat("a", 40),
	} {
		if record[key] != want {
			t.Errorf("environment[%q] = %q, want %q", key, record[key], want)
		}
	}
	if !strings.Contains(record["arg1"], "TestFakeUVProcess") {
		t.Fatalf("arguments = %#v", record)
	}
	if len(records) == 0 {
		t.Fatal("managed runner did not drain process output")
	}
}

func TestManaged_ScrubsHostSupervisionEnvironment(t *testing.T) {
	for _, key := range []string{
		autoMASUVExecutable,
		autoMASProtocol,
		autoMASVersion,
		autoMASCommit,
		autoMASSupervised,
	} {
		t.Setenv(strings.ToLower(key), "host-value")
	}
	runner := newTestRunner(t)
	environment := runner.EnvironmentForTesting(RunOptions{Environment: map[string]string{
		autoMASProtocol:                  "1",
		strings.ToLower(autoMASProtocol): "malicious-option",
		autoMASSupervised:                "1",
	}})
	for _, absent := range []string{
		autoMASUVExecutable,
		autoMASProtocol,
		autoMASVersion,
		autoMASCommit,
		autoMASSupervised,
	} {
		if containsEnvironmentKeyMap(environment, absent) {
			t.Fatalf("environment contains inherited %q", absent)
		}
	}
}

func TestManagedRunner_ScrubsTelemetryEnvironment(t *testing.T) {
	testCases := []struct {
		name     string
		identity *SupervisionIdentity
		mode     string
		offline  bool
	}{
		{name: "managed enabled", identity: &SupervisionIdentity{Version: "v6.0.0-test", Commit: strings.Repeat("a", 40)}, mode: "enabled"},
		{name: "managed disabled", identity: &SupervisionIdentity{Version: "v6.0.0-test", Commit: strings.Repeat("a", 40)}, mode: "disabled"},
		{name: "managed offline", identity: &SupervisionIdentity{Version: "v6.0.0-test", Commit: strings.Repeat("a", 40)}, mode: "enabled", offline: true},
		{name: "development enabled", mode: "enabled"},
		{name: "development disabled", mode: "disabled"},
		{name: "development offline", mode: "enabled", offline: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, key := range telemetryEnvironmentKeysForTest() {
				t.Setenv(key, "host-value")
			}
			t.Setenv("AUTO_MAS_TELEMETRY", testCase.mode)
			t.Setenv("AUTO_MAS_TEST_HOST_PASSTHROUGH", "host-value")
			runner := newTestRunner(t)
			recordPath := filepath.Join(t.TempDir(), "managed-telemetry-environment-record.txt")
			environment := map[string]string{
				"FAKE_UV_RECORD":                   recordPath,
				"AUTO_MAS_TEST_OPTION_PASSTHROUGH": "option-value",
			}
			for _, key := range telemetryEnvironmentKeysForTest() {
				environment[strings.ToLower(key)] = "option-value"
			}
			if testCase.offline {
				environment[strings.ToLower(uvOfflineEnv)] = "1"
			}
			managed, err := runner.StartManaged(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, ManagedOptions{
				RunOptions: RunOptions{
					Stage:       protocol.StageBackendSpawn,
					Environment: environment,
				},
				Identity: testCase.identity,
			}, nil)
			if err != nil {
				t.Fatalf("StartManaged() error = %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := managed.Wait(ctx)
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("Wait() = %#v, %v, want exit 0", result, err)
			}
			if err := managed.WaitEmpty(ctx); err != nil {
				t.Fatal(err)
			}
			if err := managed.Close(); err != nil {
				t.Fatal(err)
			}
			record := readTestRecord(t, recordPath)
			assertTelemetryEnvironmentAbsent(t, record)
			for key, want := range map[string]string{
				"AUTO_MAS_TEST_HOST_PASSTHROUGH":   "host-value",
				"AUTO_MAS_TEST_OPTION_PASSTHROUGH": "option-value",
				autoMASUVExecutable:                runner.Executable,
				autoMASProtocol:                    "1",
				autoMASSupervised:                  "1",
				uvManagedPythonEnv:                 "1",
				uvNoModifyPathEnv:                  "1",
				uvPythonInstallBinEnv:              "0",
			} {
				if got := record[key]; got != want {
					t.Errorf("environment[%q] = %q, want %q", key, got, want)
				}
			}
			if got := record["PATH"]; got == "" {
				t.Fatal("environment[PATH] is empty, want inherited PATH")
			}
			if testCase.offline {
				if got := record[uvOfflineEnv]; got != "1" {
					t.Errorf("environment[%q] = %q, want 1", uvOfflineEnv, got)
				}
			} else if containsEnvironmentKeyMap(record, uvOfflineEnv) {
				t.Errorf("environment contains %q, want absent", uvOfflineEnv)
			}
			if testCase.identity == nil {
				for _, key := range []string{autoMASVersion, autoMASCommit} {
					if containsEnvironmentKeyMap(record, key) {
						t.Errorf("development environment contains %q, want absent", key)
					}
				}
			} else {
				for key, want := range map[string]string{
					autoMASVersion: testCase.identity.Version,
					autoMASCommit:  testCase.identity.Commit,
				} {
					if got := record[key]; got != want {
						t.Errorf("environment[%q] = %q, want %q", key, got, want)
					}
				}
			}
		})
	}
}

func TestManaged_CancellationUsesOperationCancelled(t *testing.T) {
	runner := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	managed, err := runner.StartManaged(ctx, []string{"run"}, ManagedOptions{
		RunOptions: RunOptions{Stage: protocol.StageBackendSpawn},
	}, nil)
	if managed != nil {
		t.Fatalf("managed process = %#v, want nil", managed)
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Code() != protocol.CodeOperationCancelled {
		t.Fatalf("StartManaged() error = %T %v", err, err)
	}
}

func TestManaged_DevelopmentOmitsExpectedIdentity(t *testing.T) {
	for _, key := range []string{
		autoMASUVExecutable,
		autoMASProtocol,
		autoMASVersion,
		autoMASCommit,
		autoMASSupervised,
	} {
		t.Setenv(key, "host-stale")
		t.Setenv(strings.ToLower(key), "host-stale-lower")
	}
	runner := newTestRunner(t)
	recordPath := filepath.Join(t.TempDir(), "development-record.txt")
	projectEnvDir := filepath.Join(t.TempDir(), "development-venv")
	managed, err := runner.StartManaged(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:         protocol.StageBackendSpawn,
			ProjectEnvDir: projectEnvDir,
			Environment: map[string]string{
				"FAKE_UV_RECORD":                     recordPath,
				autoMASUVExecutable:                  "option-stale-uv",
				strings.ToLower(autoMASUVExecutable): "option-stale-uv-lower",
				autoMASProtocol:                      "999",
				strings.ToLower(autoMASProtocol):     "998",
				autoMASVersion:                       "option-stale-version",
				strings.ToLower(autoMASVersion):      "option-stale-version-lower",
				autoMASCommit:                        strings.Repeat("b", 40),
				strings.ToLower(autoMASCommit):       strings.Repeat("c", 40),
				autoMASSupervised:                    "0",
				strings.ToLower(autoMASSupervised):   "false",
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := managed.Wait(ctx)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Wait() = %#v, %v", result, err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	record := readTestRecord(t, recordPath)
	for key, want := range map[string]string{
		autoMASUVExecutable:  runner.Executable,
		autoMASProtocol:      "1",
		autoMASVersion:       "",
		autoMASCommit:        "",
		autoMASSupervised:    "1",
		uvProjectEnvironment: projectEnvDir,
	} {
		if got := record[key]; got != want {
			t.Errorf("environment[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestManaged_RejectsInvalidManagedIdentityBeforeSpawn(t *testing.T) {
	runner := newTestRunner(t)
	managed, err := runner.StartManaged(t.Context(), []string{"run"}, ManagedOptions{
		RunOptions: RunOptions{Stage: protocol.StageBackendSpawn},
		Identity:   &SupervisionIdentity{Version: "v6.0.0-test", Commit: "BAD"},
	}, nil)
	if managed != nil || err == nil {
		t.Fatalf("StartManaged() = %#v, %v, want validation error", managed, err)
	}
}

func TestManaged_StartFailureIncludesStableDiagnostics(t *testing.T) {
	runner := newTestRunner(t)
	runner.Executable = filepath.Join(t.TempDir(), "missing-uv.exe")
	managed, err := runner.StartManaged(t.Context(), []string{"run"}, ManagedOptions{
		RunOptions: RunOptions{Stage: protocol.StageBackendSpawn},
	}, nil)
	if managed != nil {
		t.Fatalf("StartManaged() process = %#v, want nil", managed)
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("StartManaged() error = %T %v, want uv Error", err, err)
	}
	details := operationErr.Details()
	if details["operation"] != "start" || details["projectDir"] != runner.ProjectDir ||
		details["projectEnvDir"] != runner.ProjectEnvDir {
		t.Fatalf("StartManaged() details = %#v, want stable start diagnostics", details)
	}
	if _, ok := details["windowsError"]; !ok {
		t.Fatalf("StartManaged() details = %#v, want windowsError", details)
	}
}
