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
	runner := newTestRunner(t)
	recordPath := filepath.Join(t.TempDir(), "development-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{"-test.run=^TestFakeUVProcess$"}, ManagedOptions{
		RunOptions: RunOptions{
			Stage: protocol.StageBackendSpawn,
			Environment: map[string]string{
				"FAKE_UV_RECORD": recordPath,
				autoMASVersion:   "malicious-version",
				autoMASCommit:    strings.Repeat("b", 40),
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
	if record[autoMASVersion] != "" || record[autoMASCommit] != "" {
		t.Fatalf("development identity leaked: %#v", record)
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
