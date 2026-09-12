package uv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// mirrorTestLock 是带两处官方前缀的锁夹具，便于断言改写确实发生。
const mirrorTestLock = `version = 1
revision = 3
requires-python = "==3.12.*"

[[package]]
name = "auto-mas"
version = "0.0.1"
source = { virtual = "." }
dependencies = [
    { name = "certifi" },
]

[[package]]
name = "certifi"
version = "2025.1.31"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/38/fc/certifi-2025.1.31-py3-none-any.whl", hash = "sha256:ca78db4565a652026a4db2bcdf68f2fb589ea80d0be70e03929ed730746b84fe", size = 166393 },
]
`

const mirrorTestPyProject = "[project]\nname = \"auto-mas\"\nrequires-python = \">=3.12,<3.13\"\n"

type mirrorSyncFixture struct {
	layout   *config.Layout
	runner   *fakeDependenciesRunner
	service  *DependenciesService
	attempts []MirrorAttempt
}

func newMirrorSyncFixture(t *testing.T) *mirrorSyncFixture {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	if err := os.WriteFile(layout.UVLockFile(), []byte(mirrorTestLock), 0o600); err != nil {
		t.Fatalf("WriteFile(uv.lock) error = %v", err)
	}
	if err := os.WriteFile(layout.PyProjectFile(), []byte(mirrorTestPyProject), 0o600); err != nil {
		t.Fatalf("WriteFile(pyproject.toml) error = %v", err)
	}
	fixture := &mirrorSyncFixture{layout: layout, runner: &fakeDependenciesRunner{}}
	service, err := NewDependenciesService(
		layout,
		fixture.runner,
		&fakeTreeRemover{},
		WithDependenciesStagingRemover(managedTreeRemover{layout: layout}),
	)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	fixture.service = service
	return fixture
}

func (f *mirrorSyncFixture) request(t *testing.T, spec mirror.PolicySpec) DependenciesRequest {
	t.Helper()
	if spec.Preferred == nil {
		spec.Preferred = map[mirror.Kind]string{}
	}
	policy, err := mirror.NewPolicy(spec)
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	request := dependencyTestRequest(f.layout)
	request.MirrorPolicy = policy
	request.Attempt = func(_ context.Context, attempt MirrorAttempt) error {
		f.attempts = append(f.attempts, attempt)
		return nil
	}
	return request
}

// stagingDir 返回本次操作的临时项目目录路径。
func (f *mirrorSyncFixture) stagingDir(t *testing.T) string {
	t.Helper()
	path, err := f.layout.DependencySyncDir(dependencyTestRequest(f.layout).OperationID)
	if err != nil {
		t.Fatalf("DependencySyncDir() error = %v", err)
	}
	return path
}

func (f *mirrorSyncFixture) assertRepositoryUntouched(t *testing.T) {
	t.Helper()
	lock, err := os.ReadFile(f.layout.UVLockFile())
	if err != nil {
		t.Fatalf("ReadFile(uv.lock) error = %v", err)
	}
	if string(lock) != mirrorTestLock {
		t.Error("repo/uv.lock changed during dependency sync")
	}
	project, err := os.ReadFile(f.layout.PyProjectFile())
	if err != nil {
		t.Fatalf("ReadFile(pyproject.toml) error = %v", err)
	}
	if string(project) != mirrorTestPyProject {
		t.Error("repo/pyproject.toml changed during dependency sync")
	}
}

func (f *mirrorSyncFixture) assertNoStagingLeftovers(t *testing.T) {
	t.Helper()
	staging := f.stagingDir(t)
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging directory %q stat error = %v, want not exist", staging, err)
	}
}

// managedTreeRemover 用真实受控删除移除临时项目目录，使测试能断言目录确已消失。
type managedTreeRemover struct{ layout *config.Layout }

func (r managedTreeRemover) RemoveTree(
	ctx context.Context,
	request filesystem.DeleteRequest,
) (filesystem.DeleteResult, error) {
	operator, err := filesystem.New(ctx, r.layout, silentDeleteAuditor{})
	if err != nil {
		return filesystem.DeleteResult{}, err
	}
	return operator.RemoveTree(ctx, request)
}

type silentDeleteAuditor struct{}

func (silentDeleteAuditor) RecordDeletion(context.Context, filesystem.DeleteAuditRecord) error {
	return nil
}

func mirrorSyncArgsFor(projectDir string) []string {
	return []string{
		"sync", "--project", projectDir, "--python", "3.12.10",
		"--frozen", "--no-default-groups", "--no-install-workspace",
	}
}

func lockedSyncArgsFor(projectDir string) []string {
	return []string{
		"sync", "--project", projectDir, "--python", "3.12.10",
		"--locked", "--no-default-groups", "--no-install-workspace",
	}
}

func TestDependencies_SyncRotatesToTheNextMirrorOnFailure(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	staging := fixture.stagingDir(t)
	var observedLocks []string
	fixture.runner.onRun = func(args []string, _ RunOptions) {
		if len(args) < 3 || args[1] != "--project" || args[2] != staging {
			return
		}
		lock, err := os.ReadFile(filepath.Join(staging, "uv.lock"))
		if err != nil {
			t.Errorf("ReadFile(staging uv.lock) error = %v", err)
			return
		}
		project, err := os.ReadFile(filepath.Join(staging, "pyproject.toml"))
		if err != nil {
			t.Errorf("ReadFile(staging pyproject.toml) error = %v", err)
			return
		}
		if string(project) != mirrorTestPyProject {
			t.Errorf("staging pyproject.toml = %q, want the repository copy", project)
		}
		observedLocks = append(observedLocks, string(lock))
	}
	fixture.runner.responses = []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: errors.New("aliyun is unreachable")},
		{},
	}

	result, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Source != "tsinghua" || result.SourceKind != mirror.KindPackageIndex.String() {
		t.Fatalf("result source = %q/%q, want tsinghua/package-index", result.Source, result.SourceKind)
	}
	if result.AttemptCount != 2 || !result.LockRewritten || !result.Synchronized {
		t.Fatalf("result = %#v, want 2 attempts with a rewritten lock", result)
	}
	if got, want := len(fixture.runner.calls), 3; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	for index := 1; index <= 2; index++ {
		if got := fixture.runner.calls[index].args; !reflect.DeepEqual(got, mirrorSyncArgsFor(staging)) {
			t.Fatalf("sync args[%d] = %#v, want %#v", index, got, mirrorSyncArgsFor(staging))
		}
	}
	if len(observedLocks) != 2 {
		t.Fatalf("observed staging locks = %d, want 2", len(observedLocks))
	}
	wants := []string{"https://mirrors.aliyun.com/pypi/", "https://pypi.tuna.tsinghua.edu.cn/"}
	for index, want := range wants {
		if !containsAll(observedLocks[index], want+"simple", want+"packages/") {
			t.Errorf("staging lock[%d] does not use %s: %q", index, want, observedLocks[index])
		}
		if containsAll(observedLocks[index], "https://pypi.org/simple") {
			t.Errorf("staging lock[%d] still contains the official index", index)
		}
	}
	wantAttempts := []MirrorAttempt{
		{SourceKind: "package-index", Source: "aliyun", SourceTry: 1, GlobalTry: 1},
		{SourceKind: "package-index", Source: "tsinghua", SourceTry: 1, GlobalTry: 2},
	}
	if !reflect.DeepEqual(fixture.attempts, wantAttempts) {
		t.Errorf("attempts = %#v, want %#v", fixture.attempts, wantAttempts)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_SyncFallsBackToTheOriginalLock(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	fixture.runner.responses = []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: errors.New("aliyun failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("tsinghua failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("ustc failed")},
		{},
	}

	result, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Source != "pypi" || result.AttemptCount != 4 || result.LockRewritten {
		t.Fatalf("result = %#v, want the official source without a rewritten lock", result)
	}
	last := fixture.runner.calls[len(fixture.runner.calls)-1]
	if got := last.args; !reflect.DeepEqual(got, lockedSyncArgsFor(fixture.layout.RepoDir())) {
		t.Fatalf("fallback args = %#v, want %#v", got, lockedSyncArgsFor(fixture.layout.RepoDir()))
	}
	if !fixture.attempts[len(fixture.attempts)-1].Fallback {
		t.Error("last attempt Fallback = false, want true")
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_SyncMirrorOnlyDoesNotFallBack(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	fixture.runner.responses = []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: errors.New("aliyun failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("tsinghua failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("ustc failed")},
	}

	_, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{MirrorOnly: true}))
	assertPythonCode(t, err, protocol.CodeMirrorExhausted)
	if got, want := len(fixture.attempts), 3; got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
	for _, attempt := range fixture.attempts {
		if attempt.Fallback || attempt.Source == "pypi" {
			t.Fatalf("mirror-only attempted the official source: %#v", attempt)
		}
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_SyncMapsExhaustedRotationWithFallbackToSyncFailure(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	fixture.runner.responses = []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: errors.New("aliyun failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("tsinghua failed")},
		{result: UVResult{ExitCode: 1}, err: errors.New("ustc failed")},
		{result: UVResult{ExitCode: 2}, err: errors.New("pypi failed")},
	}

	_, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	assertPythonCode(t, err, protocol.CodeDependencySyncFailed)
	if got, want := len(fixture.attempts), 4; got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_SyncOfflineSkipsRewriteEntirely(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	fixture.runner.responses = []fakeRunnerResponse{{}, {}}

	result, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{Offline: true}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Source != "" || result.LockRewritten || result.AttemptCount != 1 {
		t.Fatalf("result = %#v, want a single offline attempt without a source", result)
	}
	if got, want := len(fixture.runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	if got := fixture.runner.calls[1].args; !reflect.DeepEqual(got, lockedSyncArgsFor(fixture.layout.RepoDir())) {
		t.Fatalf("offline sync args = %#v, want %#v", got, lockedSyncArgsFor(fixture.layout.RepoDir()))
	}
	if got := fixture.runner.calls[1].options.Environment[uvOfflineEnv]; got != "1" {
		t.Fatalf("offline environment = %q, want 1", got)
	}
	if len(fixture.attempts) != 0 {
		t.Errorf("attempts = %#v, want none for offline", fixture.attempts)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func TestDependencies_SyncCancellationRemovesStagingProject(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	staging := fixture.stagingDir(t)
	stagingSeenOnDisk := false
	fixture.runner.onRun = func(args []string, _ RunOptions) {
		if len(args) < 3 || args[1] != "--project" || args[2] != staging {
			return
		}
		if info, err := os.Lstat(staging); err == nil && info.IsDir() {
			stagingSeenOnDisk = true
		}
		cancel()
	}
	fixture.runner.responses = []fakeRunnerResponse{
		{},
		{result: UVResult{ExitCode: 1}, err: context.Canceled},
	}

	_, err := fixture.service.Sync(ctx, fixture.request(t, mirror.PolicySpec{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync() error = %v, want context.Canceled", err)
	}
	if !stagingSeenOnDisk {
		t.Fatal("staging directory was never created; the cancellation path is not exercised")
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
