package uv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDependencies_LockfileContract(t *testing.T) {
	tests := []struct {
		name     string
		lockKind string
		wantCode protocol.Code
	}{
		{name: "missing", lockKind: "missing", wantCode: protocol.CodeLockfileMissing},
		{name: "directory", lockKind: "directory", wantCode: protocol.CodeLockfileMissing},
		{name: "outdated", lockKind: "outdated", wantCode: protocol.CodeLockfileOutdated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatalf("NewLayout() error = %v", err)
			}
			runner := &fakeDependenciesRunner{}
			lockPath := layout.UVLockFile()
			switch test.lockKind {
			case "directory":
				if err := os.MkdirAll(lockPath, 0o700); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
			case "outdated":
				writeLockfile(t, lockPath)
				runner.responses = []fakeRunnerResponse{{result: UVResult{ExitCode: 1}, err: errors.New("lock is stale")}}
			}
			service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
			if err != nil {
				t.Fatalf("NewDependenciesService() error = %v", err)
			}
			_, err = service.Check(context.Background(), dependencyTestRequest(layout))
			assertPythonCode(t, err, test.wantCode)
		})
	}
}

func TestDependencies_CheckDetectsUnsynchronizedEnvironment(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeLockfile(t, layout.UVLockFile())
	runner := &fakeDependenciesRunner{responses: []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: errors.New("environment is not synchronized")},
	}}
	service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}

	_, err = service.Check(t.Context(), dependencyTestRequest(layout))
	assertPythonCode(t, err, protocol.CodeDependencySyncFailed)
	if got, want := len(runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	want := []string{
		"sync", "--project", layout.RepoDir(), "--python", "3.12.10", "--check",
		"--locked", "--no-default-groups", "--no-install-workspace",
	}
	if got := runner.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("check sync args = %#v, want %#v", got, want)
	}
	if got := runner.calls[1].options.Environment[uvOfflineEnv]; got != "1" {
		t.Fatalf("check sync offline environment = %q, want 1", got)
	}
	if got, want := runner.calls[1].options.Stage, protocol.StageDependenciesCheck; got != want {
		t.Fatalf("check sync stage = %q, want %q", got, want)
	}
}

func TestDependencies_SyncArguments(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	fixture.runner.responses = []fakeRunnerResponse{{}, {}}

	result, err := fixture.service.Sync(context.Background(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Synchronized || !result.LockfileChecked {
		t.Fatalf("Sync() result = %#v, want checked and synchronized", result)
	}
	if result.Source != "aliyun" || result.AttemptCount != 1 || !result.LockRewritten {
		t.Fatalf("Sync() result = %#v, want the first mirror on a rewritten lock", result)
	}
	if got, want := len(fixture.runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	want := mirrorSyncArgsFor(fixture.stagingDir(t))
	if got := fixture.runner.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("sync args = %#v, want %#v", got, want)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_LockfileCheckPreservesLockSources(t *testing.T) {
	tests := []struct {
		name        string
		policySpec  mirror.PolicySpec
		wantOffline bool
	}{
		{
			name:       "default online",
			policySpec: mirror.PolicySpec{Preferred: map[mirror.Kind]string{}},
		},
		{
			name:        "offline",
			policySpec:  mirror.PolicySpec{Preferred: map[mirror.Kind]string{}, Offline: true},
			wantOffline: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMirrorSyncFixture(t)
			layout := fixture.layout
			runner := fixture.runner
			policy, err := mirror.NewPolicy(test.policySpec)
			if err != nil {
				t.Fatalf("NewPolicy() error = %v", err)
			}
			request := dependencyTestRequest(layout)
			request.MirrorPolicy = policy

			if _, err := fixture.service.Sync(t.Context(), request); err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			if got, want := len(runner.calls), 2; got != want {
				t.Fatalf("runner calls = %d, want %d", got, want)
			}
			lockCall := runner.calls[0]
			wantArgs := []string{"lock", "--project", layout.RepoDir(), "--check"}
			if got := lockCall.args; !reflect.DeepEqual(got, wantArgs) {
				t.Fatalf("lock check args = %#v, want %#v", got, wantArgs)
			}
			offlineValue, offlineSet := lockCall.options.Environment[uvOfflineEnv]
			if test.wantOffline {
				if !offlineSet || offlineValue != "1" {
					t.Fatalf("lock check offline environment = %q, %t, want 1, true", offlineValue, offlineSet)
				}
			} else if offlineSet {
				t.Fatalf("lock check offline environment = %q, true, want unset", offlineValue)
			}

			syncCall := runner.calls[1]
			// 在线时同步跑在改写后的锁副本上，离线完全不改写、沿用原锁。
			wantSyncArgs := mirrorSyncArgsFor(fixture.stagingDir(t))
			if test.wantOffline {
				wantSyncArgs = lockedSyncArgsFor(layout.RepoDir())
			}
			if got := syncCall.args; !reflect.DeepEqual(got, wantSyncArgs) {
				t.Fatalf("sync args = %#v, want %#v", got, wantSyncArgs)
			}
			syncOfflineValue, syncOfflineSet := syncCall.options.Environment[uvOfflineEnv]
			if test.wantOffline {
				if !syncOfflineSet || syncOfflineValue != "1" {
					t.Fatalf("sync offline environment = %q, %t, want 1, true", syncOfflineValue, syncOfflineSet)
				}
			} else if syncOfflineSet {
				t.Fatalf("sync offline environment = %q, true, want unset", syncOfflineValue)
			}
		})
	}
}

// TestDependencies_SyncPrefersExplicitSource 锁定 C10 的 2026-09-01 修订：
// 显式 --mirror package-index=<键> 不再是参数错误，而是把该源排在尝试顺序最前，
// 与自动轮换走同一条改写路径。显式指定官方源时第一次就用原锁。
func TestDependencies_SyncPrefersExplicitSource(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		wantRewritten bool
	}{
		{name: "mirror", key: "ustc", wantRewritten: true},
		{name: "official", key: "pypi"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMirrorSyncFixture(t)
			fixture.runner.responses = []fakeRunnerResponse{{}, {}}
			request := fixture.request(t, mirror.PolicySpec{Preferred: map[mirror.Kind]string{
				mirror.KindPackageIndex: test.key,
			}})

			result, err := fixture.service.Sync(t.Context(), request)
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			if result.Source != test.key || result.AttemptCount != 1 {
				t.Fatalf("result = %#v, want %q on the first attempt", result, test.key)
			}
			if result.LockRewritten != test.wantRewritten {
				t.Fatalf("LockRewritten = %t, want %t", result.LockRewritten, test.wantRewritten)
			}
			if got, want := len(fixture.attempts), 1; got != want {
				t.Fatalf("attempts = %d, want %d", got, want)
			}
			if fixture.attempts[0].Source != test.key {
				t.Fatalf("first attempt source = %q, want %q", fixture.attempts[0].Source, test.key)
			}
			wantArgs := mirrorSyncArgsFor(fixture.stagingDir(t))
			if !test.wantRewritten {
				wantArgs = lockedSyncArgsFor(fixture.layout.RepoDir())
			}
			if got := fixture.runner.calls[1].args; !reflect.DeepEqual(got, wantArgs) {
				t.Fatalf("sync args = %#v, want %#v", got, wantArgs)
			}
			fixture.assertRepositoryUntouched(t)
			fixture.assertNoStagingLeftovers(t)
		})
	}
}

// TestDependencies_SyncRejectsUnknownSourceKey 证明策略校验本身没有放松：
// 目录里不存在的 key 仍然是参数错误，绝不静默换成别的源。
func TestDependencies_SyncRejectsUnknownSourceKey(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	request := fixture.request(t, mirror.PolicySpec{Preferred: map[mirror.Kind]string{
		mirror.KindPackageIndex: "unknown-mirror",
	}})

	_, err := fixture.service.Sync(t.Context(), request)
	assertPythonCode(t, err, protocol.CodeInvalidArgument)
	if got, want := len(fixture.runner.calls), 1; got != want {
		t.Fatalf("runner calls = %d, want %d (lock check only)", got, want)
	}
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_OnlineSyncFailureMapsToDependencySyncFailed(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	failure := fakeRunnerResponse{result: UVResult{ExitCode: 1}, err: errors.New("download failed")}
	fixture.runner.responses = []fakeRunnerResponse{{}, failure, failure, failure, failure}

	_, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	assertPythonCode(t, err, protocol.CodeDependencySyncFailed)
	// 一次锁检查 + 三个镜像 + 一次原锁回退。
	if got, want := len(fixture.runner.calls), 5; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_OfflineFailureMapsToNetworkUnavailable(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeLockfile(t, layout.UVLockFile())
	runner := &fakeDependenciesRunner{responses: []fakeRunnerResponse{{}, {err: errors.New("cache miss")}}}
	service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}, Offline: true})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	request := dependencyTestRequest(layout)
	request.MirrorPolicy = policy
	_, err = service.Sync(t.Context(), request)
	assertPythonCode(t, err, protocol.CodeNetworkUnavailable)
	if got := runner.calls[1].options.Environment[uvOfflineEnv]; got != "1" {
		t.Fatalf("offline environment = %q, want 1", got)
	}
}

func newTestNetworkExecutor(t *testing.T) *networkExecutor {
	t.Helper()
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	rotator, err := mirror.NewRotator(mirror.WithMaxSourceAttempts(1))
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	executor, err := newNetworkExecutor(catalog, rotator)
	if err != nil {
		t.Fatalf("newNetworkExecutor() error = %v", err)
	}
	return executor
}

func TestDependencies_RebuildUsesControlledDelete(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	remover := &fakeTreeRemover{result: filesystem.DeleteResult{Removed: true}}
	service, err := NewDependenciesService(layout, &fakeDependenciesRunner{}, remover)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	result, err := service.Rebuild(context.Background(), dependencyTestRequest(layout))
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if !result.Rebuilt {
		t.Fatalf("Rebuild() result = %#v, want rebuilt", result)
	}
	if got, want := remover.request.Kind, filesystem.DeleteManagedVenv; got != want {
		t.Fatalf("delete kind = %q, want %q", got, want)
	}
	if got, want := remover.request.Target, layout.VenvDir(); got != want {
		t.Fatalf("delete target = %q, want %q", got, want)
	}
}

type fakeRunnerResponse struct {
	result UVResult
	err    error
}

type fakeDependenciesCall struct {
	args    []string
	options RunOptions
}

type fakeDependenciesRunner struct {
	responses []fakeRunnerResponse
	calls     []fakeDependenciesCall
	// onRun 在返回预置响应前观察一次调用，用于断言临时项目目录的即时状态。
	onRun func(args []string, options RunOptions)
}

func (r *fakeDependenciesRunner) Run(_ context.Context, args []string, options RunOptions) (UVResult, error) {
	r.calls = append(r.calls, fakeDependenciesCall{
		args:    append([]string(nil), args...),
		options: options,
	})
	if r.onRun != nil {
		r.onRun(append([]string(nil), args...), options)
	}
	if len(r.responses) == 0 {
		return UVResult{}, nil
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.result, response.err
}

type fakeTreeRemover struct {
	request filesystem.DeleteRequest
	result  filesystem.DeleteResult
	err     error
}

func (r *fakeTreeRemover) RemoveTree(_ context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
	r.request = request
	return r.result, r.err
}

func dependencyTestRequest(layout *config.Layout) DependenciesRequest {
	return DependenciesRequest{
		ProjectDir:    layout.RepoDir(),
		ProjectEnvDir: layout.VenvDir(),
		PythonVersion: "3.12.10",
		OperationID:   "01J00000000000000000000000",
		Branch:        "release/v5.4.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
	}
}

func writeLockfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// codedTestError 模拟 filesystem 层带专属错误码的失败（如 UNSAFE_REPARSE_POINT）。
type codedTestError struct{ code protocol.Code }

func (e *codedTestError) Error() string       { return "coded failure" }
func (e *codedTestError) Code() protocol.Code { return e.code }

// TestDependencies_RebuildKeepsUnderlyingCodeAndPath 钉住「删除失败的真实原因不能被吞」：
// venv 里有一个 reparse point 或一个被占用的文件，删除就整体中止，而 Rebuild 此前把任何
// 原因都压成 ENVIRONMENT_REBUILD_FAILED，连是哪个文件都不说——用户和维护者都无从下手。
func TestDependencies_RebuildKeepsUnderlyingCodeAndPath(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	cause := &filesystem.FileError{
		Operation: "remove",
		Path:      filepath.Join(layout.VenvDir(), "Lib", "site-packages", "locked.pyd"),
		Err:       &codedTestError{code: protocol.CodeUnsafeReparsePoint},
	}
	remover := &fakeTreeRemover{result: filesystem.DeleteResult{Partial: true}, err: cause}
	service, err := NewDependenciesService(layout, &fakeDependenciesRunner{}, remover)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}

	_, err = service.Rebuild(context.Background(), dependencyTestRequest(layout))
	if err == nil {
		t.Fatal("Rebuild() error = nil, want failure")
	}
	var coded interface {
		Code() protocol.Code
		Details() map[string]any
	}
	if !errors.As(err, &coded) {
		t.Fatalf("Rebuild() error = %T, want a coded error with details", err)
	}
	if got := coded.Code(); got != protocol.CodeUnsafeReparsePoint {
		t.Fatalf("code = %q, want %q", got, protocol.CodeUnsafeReparsePoint)
	}
	details := coded.Details()
	if got := details["path"]; got != cause.Path {
		t.Fatalf("details[path] = %v, want %v", got, cause.Path)
	}
	if got := details["operation"]; got != "remove" {
		t.Fatalf("details[operation] = %v, want remove", got)
	}
	if details["partial"] != true {
		t.Fatalf("details[partial] = %v, want true", details["partial"])
	}
}
