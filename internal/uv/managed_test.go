package uv

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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

// TestManaged_WorkingDirOverridesProjectDir 证明 C6 的显式工作目录字段：
// 受管子进程的 cwd 由 WorkingDir 决定，而不再固定跟随 ProjectDir。
func TestManaged_WorkingDirOverridesProjectDir(t *testing.T) {
	runner := newTestRunner(t)
	workingDir := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "managed-workingdir-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:       protocol.StageBackendSpawn,
			WorkingDir:  workingDir,
			Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
		},
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
	if got, want := record["cwd"], filepath.Clean(workingDir); got != want {
		t.Fatalf("child cwd = %q, want %q", got, want)
	}
	if got := record["cwd"]; got == filepath.Clean(runner.ProjectDir) {
		t.Fatalf("child cwd = %q, want a directory other than the project dir", got)
	}
}

// TestManaged_WorkingDirDefaultsToProjectDir 锁定 development 的既有行为：
// 不传 WorkingDir 时 cwd 必须仍是 ProjectDir。
func TestManaged_WorkingDirDefaultsToProjectDir(t *testing.T) {
	runner := newTestRunner(t)
	projectDir := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "managed-default-workingdir-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:       protocol.StageBackendSpawn,
			ProjectDir:  projectDir,
			Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
		},
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
	if got, want := record["cwd"], filepath.Clean(projectDir); got != want {
		t.Fatalf("child cwd = %q, want %q", got, want)
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

// TestManaged_InjectsInfrastructureAndMirrorSources 锁定增补 1 C11 的四个注入变量：
// 两个目录是规范化绝对路径且与 uv 自己使用的目录同源，两个镜像列表按 `;` 保序。
func TestManaged_InjectsInfrastructureAndMirrorSources(t *testing.T) {
	runner := newTestRunner(t)
	packageIndex := []string{
		"https://mirrors.aliyun.com/pypi/simple/",
		"https://pypi.tuna.tsinghua.edu.cn/simple/",
		"https://pypi.org/simple/",
	}
	pythonSources := []string{
		"https://gh-proxy.com/https://github.com/astral-sh/python-build-standalone/releases/download",
		"https://github.com/astral-sh/python-build-standalone/releases/download",
	}
	recordPath := filepath.Join(t.TempDir(), "managed-infrastructure-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:       protocol.StageBackendSpawn,
			Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
		},
		Infrastructure: SupervisionInfrastructure{
			// 刻意传未清理的形态，证明注入值经过 filepath.Clean。
			UVCacheDir:          filepath.Join(runner.CacheDir, "sub", ".."),
			PythonInstallDir:    runner.PythonInstallDir,
			PackageIndexSources: packageIndex,
			PythonSources:       pythonSources,
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartManaged() error = %v", err)
	}
	waitManagedProcess(t, managed)
	record := readTestRecord(t, recordPath)
	for key, want := range map[string]string{
		autoMASUVCacheDir:         filepath.Clean(runner.CacheDir),
		autoMASUVPythonInstallDir: filepath.Clean(runner.PythonInstallDir),
		autoMASMirrorPackageIndex: strings.Join(packageIndex, ";"),
		autoMASMirrorPython:       strings.Join(pythonSources, ";"),
	} {
		if got := record[key]; got != want {
			t.Errorf("environment[%q] = %q, want %q", key, got, want)
		}
	}
	// C11 的目的是「共用一份缓存与一份解释器」：下发值一旦与 uv 自己用的目录
	// 分叉，这条契约就名存实亡，因此把等式本身锁进测试。
	if record[autoMASUVCacheDir] != record[uvCacheDirEnv] {
		t.Errorf("AUTO_MAS_UV_CACHE_DIR = %q, want the same value as UV_CACHE_DIR %q",
			record[autoMASUVCacheDir], record[uvCacheDirEnv])
	}
	if record[autoMASUVPythonInstallDir] != record[uvPythonInstallDirEnv] {
		t.Errorf("AUTO_MAS_UV_PYTHON_INSTALL_DIR = %q, want the same value as UV_PYTHON_INSTALL_DIR %q",
			record[autoMASUVPythonInstallDir], record[uvPythonInstallDirEnv])
	}
	if got := strings.Split(record[autoMASMirrorPackageIndex], ";"); !slices.Equal(got, packageIndex) {
		t.Errorf("package index sources = %#v, want %#v in plan order", got, packageIndex)
	}
}

// TestManaged_InjectsEmptyMirrorListsWhenOffline 锁定 --offline 的取值形态：
// 键必须存在且为空串，而不是缺席——契约表两列都写「必填」。
func TestManaged_InjectsEmptyMirrorListsWhenOffline(t *testing.T) {
	runner := newTestRunner(t)
	recordPath := filepath.Join(t.TempDir(), "managed-offline-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:       protocol.StageBackendSpawn,
			Environment: map[string]string{"FAKE_UV_RECORD": recordPath},
		},
		Infrastructure: SupervisionInfrastructure{
			UVCacheDir:       runner.CacheDir,
			PythonInstallDir: runner.PythonInstallDir,
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartManaged() error = %v", err)
	}
	waitManagedProcess(t, managed)
	record := readTestRecord(t, recordPath)
	for _, key := range []string{autoMASMirrorPackageIndex, autoMASMirrorPython} {
		value, ok := record[key]
		if !ok {
			t.Errorf("environment is missing %q, want the key with an empty value", key)
			continue
		}
		if value != "" {
			t.Errorf("environment[%q] = %q, want an empty string", key, value)
		}
	}
}

// TestManaged_InfrastructureKeysAreControlled 证明四个键属于受控监督集合：
// 宿主与 RunOptions.Environment 里的同名项（含大小写变体）都被清除并被受控值覆盖。
func TestManaged_InfrastructureKeysAreControlled(t *testing.T) {
	runner := newTestRunner(t)
	t.Setenv(autoMASUVCacheDir, `C:\host\stale-cache`)
	t.Setenv(autoMASMirrorPackageIndex, "https://host.example/simple/")
	recordPath := filepath.Join(t.TempDir(), "managed-controlled-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage: protocol.StageBackendSpawn,
			Environment: map[string]string{
				"FAKE_UV_RECORD":                           recordPath,
				strings.ToLower(autoMASUVCacheDir):         `C:\option\stale-cache`,
				strings.ToLower(autoMASUVPythonInstallDir): `C:\option\stale-python`,
				autoMASMirrorPackageIndex:                  "https://option.example/simple/",
				autoMASMirrorPython:                        "https://option.example/python",
			},
		},
		Infrastructure: SupervisionInfrastructure{
			UVCacheDir:          runner.CacheDir,
			PythonInstallDir:    runner.PythonInstallDir,
			PackageIndexSources: []string{"https://pypi.org/simple/"},
			PythonSources:       []string{"https://github.com/astral-sh/python-build-standalone/releases/download"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartManaged() error = %v", err)
	}
	waitManagedProcess(t, managed)
	record := readTestRecord(t, recordPath)
	for key, want := range map[string]string{
		autoMASUVCacheDir:         filepath.Clean(runner.CacheDir),
		autoMASUVPythonInstallDir: filepath.Clean(runner.PythonInstallDir),
		autoMASMirrorPackageIndex: "https://pypi.org/simple/",
		autoMASMirrorPython:       "https://github.com/astral-sh/python-build-standalone/releases/download",
	} {
		if got := record[key]; got != want {
			t.Errorf("environment[%q] = %q, want the controlled value %q", key, got, want)
		}
	}
	for key := range record {
		if strings.EqualFold(key, autoMASUVCacheDir) && key != autoMASUVCacheDir {
			t.Errorf("environment contains case variant %q of a controlled supervision key", key)
		}
	}
}

// TestManaged_InfrastructureFallsBackToResolvedDirectories 证明调用方不传目录时
// 回退到本次 uv 调用实际解析出的受管目录，而不是留空破坏契约。
func TestManaged_InfrastructureFallsBackToResolvedDirectories(t *testing.T) {
	runner := newTestRunner(t)
	overrideCache := t.TempDir()
	overridePython := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "managed-fallback-record.txt")
	managed, err := runner.StartManaged(t.Context(), []string{
		"-test.run=^TestFakeUVProcess$",
	}, ManagedOptions{
		RunOptions: RunOptions{
			Stage:            protocol.StageBackendSpawn,
			CacheDir:         overrideCache,
			PythonInstallDir: overridePython,
			Environment:      map[string]string{"FAKE_UV_RECORD": recordPath},
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartManaged() error = %v", err)
	}
	waitManagedProcess(t, managed)
	record := readTestRecord(t, recordPath)
	if got, want := record[autoMASUVCacheDir], filepath.Clean(overrideCache); got != want {
		t.Errorf("AUTO_MAS_UV_CACHE_DIR = %q, want the resolved cache dir %q", got, want)
	}
	if got, want := record[autoMASUVPythonInstallDir], filepath.Clean(overridePython); got != want {
		t.Errorf("AUTO_MAS_UV_PYTHON_INSTALL_DIR = %q, want the resolved python install dir %q", got, want)
	}
}

// TestManaged_RejectsInvalidInfrastructure 覆盖失败关闭：相对目录与含 `;` 的源
// 都在 spawn 之前被拒绝，绝不下发一个调用方无法正确切分的列表。
func TestManaged_RejectsInvalidInfrastructure(t *testing.T) {
	tests := []struct {
		name           string
		infrastructure SupervisionInfrastructure
	}{
		{
			name:           "relative cache dir",
			infrastructure: SupervisionInfrastructure{UVCacheDir: `relative\cache`},
		},
		{
			name:           "relative python install dir",
			infrastructure: SupervisionInfrastructure{PythonInstallDir: `relative\python`},
		},
		{
			name: "package index source contains separator",
			infrastructure: SupervisionInfrastructure{
				PackageIndexSources: []string{"https://mirror.example/simple/;https://other.example/simple/"},
			},
		},
		{
			name:           "python source is empty",
			infrastructure: SupervisionInfrastructure{PythonSources: []string{""}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newTestRunner(t)
			managed, err := runner.StartManaged(t.Context(), []string{"run"}, ManagedOptions{
				RunOptions:     RunOptions{Stage: protocol.StageBackendSpawn},
				Infrastructure: test.infrastructure,
			}, nil)
			if managed != nil || err == nil {
				t.Fatalf("StartManaged() = %#v, %v, want validation error", managed, err)
			}
		})
	}
}

func waitManagedProcess(t *testing.T, managed *process.ManagedProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := managed.Wait(ctx)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Wait() = %#v, %v, want exit 0", result, err)
	}
	if err := managed.WaitEmpty(ctx); err != nil {
		t.Fatalf("WaitEmpty() error = %v", err)
	}
	if err := managed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
