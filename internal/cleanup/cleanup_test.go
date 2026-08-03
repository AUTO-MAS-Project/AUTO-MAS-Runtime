package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const testOperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// memoryAuditor 记录 filesystem 审计事件，供断言使用。
type memoryAuditor struct {
	mu      sync.Mutex
	records []filesystem.DeleteAuditRecord
}

func (a *memoryAuditor) RecordDeletion(
	ctx context.Context,
	record filesystem.DeleteAuditRecord,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, record)
	return nil
}

func (a *memoryAuditor) recordsCopy() []filesystem.DeleteAuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]filesystem.DeleteAuditRecord(nil), a.records...)
}

func newTestLayout(t *testing.T) (*config.Layout, string) {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return layout, root
}

func newTestService(t *testing.T, layout *config.Layout) (*Service, *memoryAuditor) {
	t.Helper()
	auditor := &memoryAuditor{}
	service, err := New(layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service, auditor
}

func newTestEmitter(t *testing.T) (*protocol.Emitter, *strings.Builder) {
	t.Helper()
	var stdout strings.Builder
	output, err := protocol.NewProcessOutput(&stdout)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"dev",
		"cleanup",
		[]string{},
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	return emitter, &stdout
}

func runService(t *testing.T, service *Service) (Report, error) {
	t.Helper()
	emitter, _ := newTestEmitter(t)
	return service.Run(context.Background(), emitter)
}

func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func findItem(t *testing.T, report Report, id string) Item {
	t.Helper()
	for _, item := range report.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("report has no item %q: %+v", id, report)
	return Item{}
}

// mustCreateJunction 用 pwsh 创建目录 Junction，只移除链接本身。
func mustCreateJunction(t *testing.T, link, target string) {
	t.Helper()
	script := `$ErrorActionPreference = 'Stop'
$target = $env:AUTO_MAS_CLEANUP_JUNCTION_TARGET
$path = $env:AUTO_MAS_CLEANUP_JUNCTION_PATH
if ([string]::IsNullOrWhiteSpace($target) -or
    [string]::IsNullOrWhiteSpace($path)) {
    throw 'cleanup junction environment is incomplete'
}
New-Item -ItemType Junction -Path $path -Target $target -ErrorAction Stop |
    Out-Null`
	command := exec.CommandContext(
		t.Context(),
		"pwsh",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_CLEANUP_JUNCTION_TARGET="+target,
		"AUTO_MAS_CLEANUP_JUNCTION_PATH="+link,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create cleanup Junction: %v; output=%q", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove Junction %q: %v", link, err)
		}
	})
}

// deadPID 返回一个已确认退出的进程 PID。
func deadPID(t *testing.T) uint32 {
	t.Helper()
	command := exec.CommandContext(
		t.Context(),
		"pwsh",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		"Start-Sleep -Milliseconds 50",
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process error = %v", err)
	}
	pid := uint32(command.Process.Pid)
	if err := command.Wait(); err != nil {
		t.Fatalf("wait helper process error = %v", err)
	}
	return pid
}

// writeUpdateTransaction 通过 state.Store 写入一条指定 operationID 的 update 事务。
func writeUpdateTransaction(t *testing.T, layout *config.Layout, operationID string) {
	t.Helper()
	store, err := state.NewStore(context.Background(), layout)
	if err != nil {
		t.Fatalf("state.NewStore() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	}()
	value, err := store.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID:   operationID,
		Command:       "workspace sync",
		PID:           deadPID(t),
		TargetVersion: "v5.4.0-beta.1",
		Stage:         protocol.StageWorkspaceClone,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(
		context.Background(),
		state.TransactionUpdate,
		value,
	); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
}

func TestCleanup_RemovesUVCacheAndDownloadTemp(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	writeFixtureFile(t, filepath.Join(layout.DownloadCacheDir(), "temp.bin"), []byte("y"))
	service, auditor := newTestService(t, layout)

	report, err := runService(t, service)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Summary.Cleaned != 2 || report.Summary.Skipped != 1 || report.Summary.Failed != 0 {
		t.Fatalf("summary = %+v, want 2 cleaned and 1 skipped", report.Summary)
	}
	if dirExists(layout.UVCacheDir()) {
		t.Error("uv cache dir still exists")
	}
	if dirExists(layout.DownloadCacheDir()) {
		t.Error("download cache dir still exists")
	}
	if len(auditor.recordsCopy()) < 4 {
		t.Errorf("audit records = %d, want at least 4 (2 started + 2 finished)", len(auditor.recordsCopy()))
	}
}

func TestCleanup_RemovesPythonCacheUnderRepo(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.RepoDir(), "__pycache__", "a.pyc"), []byte("a"))
	writeFixtureFile(t, filepath.Join(layout.RepoDir(), "app", "__pycache__", "b.pyc"), []byte("b"))
	writeFixtureFile(t, filepath.Join(layout.RepoDir(), "app", "main.py"), []byte("keep"))
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Summary.Cleaned != 2 {
		t.Fatalf("summary = %+v, want 2 python caches cleaned", report.Summary)
	}
	if dirExists(filepath.Join(layout.RepoDir(), "__pycache__")) {
		t.Error("root __pycache__ still exists")
	}
	if dirExists(filepath.Join(layout.RepoDir(), "app", "__pycache__")) {
		t.Error("nested __pycache__ still exists")
	}
	if !fileExists(filepath.Join(layout.RepoDir(), "app", "main.py")) {
		t.Error("repo source file was removed")
	}
}

// TestCleanup_RemovesBuildCache 证明 build-cache 条目：有内容时删除，
// 目录不存在时 skipped，且不触碰保护名单目录。
func TestCleanup_RemovesBuildCache(t *testing.T) {
	t.Parallel()
	t.Run("with content removes", func(t *testing.T) {
		t.Parallel()
		layout, _ := newTestLayout(t)
		writeFixtureFile(t, filepath.Join(layout.BuildCacheDir(), "build.bin"), []byte("x"))
		service, _ := newTestService(t, layout)

		report, err := runService(t, service)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		item := findItem(t, report, "build-cache")
		if item.Status != ItemCleaned {
			t.Fatalf("build-cache status = %q, want cleaned: %+v", item.Status, item)
		}
		if dirExists(layout.BuildCacheDir()) {
			t.Error("build cache dir still exists")
		}
	})
	t.Run("absent skipped", func(t *testing.T) {
		t.Parallel()
		layout, _ := newTestLayout(t)
		service, _ := newTestService(t, layout)

		report, err := runService(t, service)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		item := findItem(t, report, "build-cache")
		if item.Status != ItemSkipped {
			t.Fatalf("build-cache status = %q, want skipped: %+v", item.Status, item)
		}
	})
	t.Run("does not touch protected dirs", func(t *testing.T) {
		t.Parallel()
		layout, _ := newTestLayout(t)
		writeFixtureFile(t, filepath.Join(layout.BuildCacheDir(), "build.bin"), []byte("x"))
		sentinel := filepath.Join(layout.ConfigDir(), "keep.txt")
		writeFixtureFile(t, sentinel, []byte("user data"))
		service, _ := newTestService(t, layout)

		if _, err := runService(t, service); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !fileExists(sentinel) {
			t.Error("protected config sentinel was removed")
		}
	})
}

func TestCleanup_EmptyRootIdempotentSuccess(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	service, _ := newTestService(t, layout)

	first, err := runService(t, service)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if first.Summary.Failed != 0 {
		t.Fatalf("first summary = %+v, want no failures", first.Summary)
	}
	second, err := runService(t, service)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if second.Summary.Failed != 0 {
		t.Fatalf("second summary = %+v, want no failures", second.Summary)
	}
}

// TestService_MissingAppRootRejectedAsInvalidArgument 证明 app-root 不存在时
// 不再返回 MUTEX_OPERATION_FAILED + retryable=true：真实原因是调用方给的
// --app-root 指向不存在的目录，重试永远不会成功，可重试标志会驱动 Electron
// 陷入无意义重试循环。改为 INVALID_ARGUMENT（exit 2、不可重试、run-doctor）。
func TestService_MissingAppRootRejectedAsInvalidArgument(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist")
	layout, err := config.NewLayout(missing, base)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want INVALID_ARGUMENT")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeInvalidArgument {
		t.Errorf("error code = %q, want INVALID_ARGUMENT", cleanupErr.Code())
	}
	definition, ok := protocol.LookupErrorDefinition(cleanupErr.Code())
	if !ok {
		t.Fatalf("code %q has no frozen definition", cleanupErr.Code())
	}
	if definition.Retryable {
		t.Errorf("code %q is retryable; a missing app-root never becomes present on retry", cleanupErr.Code())
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("app-root was created as a side effect: stat error = %v", statErr)
	}
}

// TestService_AppRootNotDirectoryRejectedAsInvalidArgument 覆盖 app-root
// 存在但不是目录的情形，走同一条参数错误路径。
func TestService_AppRootNotDirectoryRejectedAsInvalidArgument(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	file := filepath.Join(base, "not-a-dir")
	writeFixtureFile(t, file, []byte("x"))
	layout, err := config.NewLayout(file, base)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want INVALID_ARGUMENT")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeInvalidArgument {
		t.Errorf("error code = %q, want INVALID_ARGUMENT", cleanupErr.Code())
	}
	if !fileExists(file) {
		t.Error("app-root file was replaced or removed")
	}
}

func TestCleanup_PreservesUserDataAndPlugins(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	protected := map[string]string{
		"config":  layout.ConfigDir(),
		"data":    layout.DataDir(),
		"history": layout.HistoryDir(),
		"script":  layout.ScriptDir(),
		"debug":   layout.DebugDir(),
		"plugins": layout.PluginsDir(),
	}
	sentinels := make(map[string]string, len(protected))
	for name, dir := range protected {
		path := filepath.Join(dir, "keep.txt")
		writeFixtureFile(t, path, []byte("user data"))
		sentinels[name] = path
	}
	service, _ := newTestService(t, layout)

	if _, err := runService(t, service); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for name, path := range sentinels {
		if !fileExists(path) {
			t.Errorf("protected %s sentinel missing: %s", name, path)
		}
	}
	if dirExists(layout.UVCacheDir()) {
		t.Error("uv cache dir still exists")
	}
}

func TestCleanup_PreservesDiagnosticLogs(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	logFile := filepath.Join(layout.RuntimeLogDir(), "backend-20260802.log")
	writeFixtureFile(t, logFile, []byte("diagnostic"))
	writeFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	service, _ := newTestService(t, layout)

	if _, err := runService(t, service); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !fileExists(logFile) {
		t.Error("runtime diagnostic log was removed")
	}
}

func TestCleanup_RejectsJunctionPointingToUserData(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.DataDir(), "keep.txt"), []byte("user data"))
	mustCreateJunction(t, layout.UVCacheDir(), layout.DataDir())
	service, _ := newTestService(t, layout)

	_, err := runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want cleanup failure")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeGitRepoCleanupFailed &&
		cleanupErr.Code() != protocol.CodeUnsafeReparsePoint {
		t.Errorf(
			"error code = %q, want GIT_REPO_CLEANUP_FAILED or UNSAFE_REPARSE_POINT",
			cleanupErr.Code(),
		)
	}
	if !fileExists(filepath.Join(layout.DataDir(), "keep.txt")) {
		t.Error("user data was removed through junction")
	}
	if !dirExists(layout.UVCacheDir()) {
		t.Error("junction was removed; fail-closed requires it to remain")
	}
}

// TestService_JunctionPycacheRecordedAsFailed 证明名为 __pycache__ 的
// Junction 不再被静默跳过：不跟随、不删除，记录稳定 failed 条目
// python-cache-invalid-1，命令以 GIT_REPO_CLEANUP_FAILED 失败，progress
// 发射 failed，用户数据与 junction 均原封不动。
func TestService_JunctionPycacheRecordedAsFailed(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	sentinel := filepath.Join(layout.DataDir(), "keep.txt")
	writeFixtureFile(t, sentinel, []byte("user data"))
	mustCreateJunction(t, filepath.Join(layout.RepoDir(), "__pycache__"), layout.DataDir())
	service, _ := newTestService(t, layout)
	emitter, stdout := newTestEmitter(t)

	report, err := service.Run(context.Background(), emitter)
	if err == nil {
		t.Fatal("Run() error = nil, want GIT_REPO_CLEANUP_FAILED")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeGitRepoCleanupFailed {
		t.Errorf("error code = %q, want GIT_REPO_CLEANUP_FAILED", cleanupErr.Code())
	}
	item := findItem(t, report, "python-cache-invalid-1")
	if item.Status != ItemFailed {
		t.Errorf("item status = %q, want failed: %+v", item.Status, item)
	}
	if code, _ := item.Details["code"].(string); code != string(protocol.CodeGitRepoCleanupFailed) {
		t.Errorf("item details code = %q, want GIT_REPO_CLEANUP_FAILED", code)
	}
	if !dirExists(filepath.Join(layout.RepoDir(), "__pycache__")) {
		t.Error("__pycache__ junction was removed; fail-closed requires it to remain")
	}
	if !fileExists(sentinel) {
		t.Error("user data was removed through the junction")
	}
	progress := stdout.String()
	if !strings.Contains(progress, "python-cache-invalid-1") {
		t.Errorf("progress output lacks invalid item id: %s", progress)
	}
	if !strings.Contains(progress, `"status":"failed"`) {
		t.Errorf("progress output lacks failed event: %s", progress)
	}
}

// TestService_JunctionPycacheNotFollowed 证明枚举遍历没有进入
// __pycache__ Junction 的目标目录：目标内嵌套的 __pycache__ 不被发现、
// 内容不被删除，junction 本身保持存在。
func TestService_JunctionPycacheNotFollowed(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	target := layout.DataDir()
	writeFixtureFile(t, filepath.Join(target, "__pycache__", "inner.pyc"), []byte("inner"))
	mustCreateJunction(t, filepath.Join(layout.RepoDir(), "__pycache__"), target)
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want GIT_REPO_CLEANUP_FAILED")
	}
	if !fileExists(filepath.Join(target, "__pycache__", "inner.pyc")) {
		t.Error("traversal entered the junction target and removed content")
	}
	if !dirExists(filepath.Join(layout.RepoDir(), "__pycache__")) {
		t.Error("__pycache__ junction was removed")
	}
	for _, item := range report.Items {
		if item.ID == "python-cache-1" {
			t.Errorf("traversal entered junction target and enumerated %q", item.ID)
		}
	}
}

// TestService_JunctionRepoUpdateRecordedAsFailed 证明名为
// repo.update-<合法 operationId> 的 Junction 不再被静默忽略：Windows 下
// Junction 的 DirEntry.IsDir() 为 false（ModeIrregular），此前被
// enumerateRepoUpdates 的 !entry.IsDir() 过滤掉，命令 exit 0、无条目、
// 无审计记录。现按失败关闭语义记为 repo-update-invalid-<n>。
func TestService_JunctionRepoUpdateRecordedAsFailed(t *testing.T) {
	t.Parallel()
	layout, root := newTestLayout(t)
	sentinel := filepath.Join(layout.DataDir(), "keep.txt")
	writeFixtureFile(t, sentinel, []byte("user data"))
	link := filepath.Join(root, "repo.update-"+testOperationID)
	mustCreateJunction(t, link, layout.DataDir())
	service, _ := newTestService(t, layout)
	emitter, stdout := newTestEmitter(t)

	report, err := service.Run(context.Background(), emitter)
	if err == nil {
		t.Fatal("Run() error = nil, want GIT_REPO_CLEANUP_FAILED")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeGitRepoCleanupFailed {
		t.Errorf("error code = %q, want GIT_REPO_CLEANUP_FAILED", cleanupErr.Code())
	}
	item := findItem(t, report, "repo-update-invalid-1")
	if item.Status != ItemFailed {
		t.Errorf("item status = %q, want failed: %+v", item.Status, item)
	}
	if code, _ := item.Details["code"].(string); code != string(protocol.CodeGitRepoCleanupFailed) {
		t.Errorf("item details code = %q, want GIT_REPO_CLEANUP_FAILED", code)
	}
	if !dirExists(link) {
		t.Error("repo.update junction was removed; fail-closed requires it to remain")
	}
	if !fileExists(sentinel) {
		t.Error("user data was removed through the junction")
	}
	progress := stdout.String()
	if !strings.Contains(progress, "repo-update-invalid-1") {
		t.Errorf("progress output lacks invalid item id: %s", progress)
	}
	if !strings.Contains(progress, `"status":"failed"`) {
		t.Errorf("progress output lacks failed event: %s", progress)
	}
}

// TestService_JunctionRepoUpdateNotFollowed 证明枚举没有跟随 Junction 进入
// 用户数据目录：目标内容原封不动，且不产生指向目标的删除审计记录。
func TestService_JunctionRepoUpdateNotFollowed(t *testing.T) {
	t.Parallel()
	layout, root := newTestLayout(t)
	target := layout.DataDir()
	nested := filepath.Join(target, "nested", "payload.txt")
	writeFixtureFile(t, nested, []byte("payload"))
	link := filepath.Join(root, "repo.update-"+testOperationID)
	mustCreateJunction(t, link, target)
	service, auditor := newTestService(t, layout)

	if _, err := runService(t, service); err == nil {
		t.Fatal("Run() error = nil, want GIT_REPO_CLEANUP_FAILED")
	}
	if !fileExists(nested) {
		t.Error("traversal entered the junction target and removed content")
	}
	if !dirExists(link) {
		t.Error("repo.update junction was removed")
	}
	for _, record := range auditor.recordsCopy() {
		if strings.Contains(record.Target, "repo.update-") {
			t.Errorf("audit recorded a deletion for the junction: %+v", record)
		}
	}
}

// TestService_NonRepoUpdateJunctionIgnored 证明 app-root 下与
// repo.update- 前缀无关的 Junction 保持静默跳过，不产生条目、不使命令失败。
func TestService_NonRepoUpdateJunctionIgnored(t *testing.T) {
	t.Parallel()
	layout, root := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.DataDir(), "keep.txt"), []byte("user data"))
	mustCreateJunction(t, filepath.Join(root, "unrelated-link"), layout.DataDir())
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	for _, item := range report.Items {
		if strings.HasPrefix(item.ID, "repo-update-") {
			t.Errorf("unrelated junction produced item %q", item.ID)
		}
	}
}

// TestService_JunctionPycacheDoesNotHideSiblings 证明 __pycache__ Junction
// 不会让同级后续条目被整体跳过。fs.SkipDir 作用于非目录条目时，WalkDir 的
// 语义是「跳过所在目录的剩余条目」——Junction 的 DirEntry.IsDir() 为 false，
// 因此对它返回 SkipDir 会连带吞掉排在后面的真实 __pycache__。
func TestService_JunctionPycacheDoesNotHideSiblings(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.DataDir(), "keep.txt"), []byte("user data"))
	// zzz 在字典序上排在 __pycache__ 之后，遍历时后被访问。
	sibling := filepath.Join(layout.RepoDir(), "zzz", "__pycache__")
	writeFixtureFile(t, filepath.Join(sibling, "module.pyc"), []byte("cache"))
	mustCreateJunction(t, filepath.Join(layout.RepoDir(), "__pycache__"), layout.DataDir())
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want GIT_REPO_CLEANUP_FAILED")
	}
	if item := findItem(t, report, "python-cache-invalid-1"); item.Status != ItemFailed {
		t.Errorf("junction item status = %q, want failed", item.Status)
	}
	if item := findItem(t, report, "python-cache-1"); item.Status != ItemCleaned {
		t.Errorf("sibling cache item status = %q, want cleaned: %+v", item.Status, item)
	}
	if dirExists(sibling) {
		t.Error("sibling __pycache__ was not cleaned")
	}
	if !fileExists(filepath.Join(layout.DataDir(), "keep.txt")) {
		t.Error("user data was removed through the junction")
	}
}

func TestCleanup_RejectsTargetOutsideManagedRoot(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	outside := t.TempDir()
	writeFixtureFile(t, filepath.Join(outside, "keep.txt"), []byte("outside"))
	mustCreateJunction(t, layout.UVCacheDir(), outside)
	service, _ := newTestService(t, layout)

	if _, err := runService(t, service); err == nil {
		t.Fatal("Run() error = nil, want cleanup failure")
	}
	if !fileExists(filepath.Join(outside, "keep.txt")) {
		t.Error("outside-root directory was modified")
	}
}

func TestCleanup_MutationLockConflict(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	heldSet, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		t.Fatalf("lock.NewSet() error = %v", err)
	}
	acquisition, err := heldSet.AcquireMutation(context.Background())
	if err != nil {
		t.Fatalf("AcquireMutation() error = %v", err)
	}
	defer func() {
		if err := acquisition.Lease().Close(); err != nil {
			t.Errorf("lease.Close() error = %v", err)
		}
		if err := heldSet.Close(); err != nil {
			t.Errorf("set.Close() error = %v", err)
		}
	}()

	service, _ := newTestService(t, layout)
	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want mutation conflict")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeMutationInProgress {
		t.Errorf("error code = %q, want MUTATION_IN_PROGRESS", cleanupErr.Code())
	}
}

func TestCleanup_BackendRunningConflict(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	heldSet, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		t.Fatalf("lock.NewSet() error = %v", err)
	}
	acquisition, err := heldSet.AcquireBackend(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackend() error = %v", err)
	}
	defer func() {
		if err := acquisition.Lease().Close(); err != nil {
			t.Errorf("lease.Close() error = %v", err)
		}
		if err := heldSet.Close(); err != nil {
			t.Errorf("set.Close() error = %v", err)
		}
	}()

	service, _ := newTestService(t, layout)
	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want backend still running")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeBackendStillRunning {
		t.Errorf("error code = %q, want BACKEND_STILL_RUNNING", cleanupErr.Code())
	}
}

func TestCleanup_RepoUpdateMatchesStateAndRemoves(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	writeUpdateTransaction(t, layout, testOperationID)
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	item := findItem(t, report, "repo-update-"+testOperationID)
	if item.Status != ItemCleaned {
		t.Fatalf("repo-update item status = %q, want cleaned: %+v", item.Status, item)
	}
	if dirExists(updateDir) {
		t.Error("stale repo.update directory still exists")
	}
}

func TestCleanup_RepoUpdateIdentityMismatchFailClosed(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	writeUpdateTransaction(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAW")
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want identity mismatch failure")
	}
	if !dirExists(updateDir) {
		t.Error("repo.update directory was removed despite identity mismatch")
	}
}

func TestCleanup_RepoUpdateNoTransactionFailClosed(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want missing-transaction failure")
	}
	if !dirExists(updateDir) {
		t.Error("repo.update directory was removed without state match")
	}
}

// TestCleanup_InvalidRepoUpdateEntriesHaveUniqueIDs 证明多个非法
// repo.update-* 目录产生两两不同的稳定条目 id，且全部失败关闭。
func TestCleanup_InvalidRepoUpdateEntriesHaveUniqueIDs(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	invalidDirs := []string{
		filepath.Join(layout.AppRoot(), "repo.update-"),
		filepath.Join(layout.AppRoot(), "repo.update-"+strings.Repeat("x", 200)),
	}
	for _, dir := range invalidDirs {
		writeFixtureFile(t, filepath.Join(dir, "partial"), []byte("stale"))
	}
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want invalid entry failure")
	}
	seen := make(map[string]bool)
	count := 0
	for _, item := range report.Items {
		if !strings.HasPrefix(item.ID, "repo-update-invalid") {
			continue
		}
		count++
		if seen[item.ID] {
			t.Errorf("duplicate item id %q", item.ID)
		}
		seen[item.ID] = true
		if item.Status != ItemFailed {
			t.Errorf("item %q status = %q, want failed", item.ID, item.Status)
		}
	}
	if count != 2 {
		t.Fatalf("invalid items = %d, want 2: %+v", count, report.Items)
	}
}

// TestCleanup_NoTransactionDoesNotCreateRuntimeState 证明无事务的
// repo.update-* 失败关闭路径不会创建 runtime-state/ 或任何根目录条目。
func TestCleanup_NoTransactionDoesNotCreateRuntimeState(t *testing.T) {
	t.Parallel()
	layout, root := newTestLayout(t)
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	before := readRootEntries(t, root)
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want no-transaction failure")
	}
	if !dirExists(updateDir) {
		t.Error("repo.update directory was removed without state match")
	}
	if dirExists(layout.StateDir()) {
		t.Error("runtime-state directory was created by fail-closed cleanup")
	}
	after := readRootEntries(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("root entries changed:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestCleanup_PartialFailureReportsSideEffects(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	writeFixtureFile(t, filepath.Join(layout.RepoDir(), "__pycache__", "a.pyc"), []byte("a"))
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	service, _ := newTestService(t, layout)

	_, err = runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want partial failure")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	details := cleanupErr.Details()
	items, ok := details["items"].([]map[string]any)
	if !ok {
		t.Fatalf("error details items = %#v, want array", details["items"])
	}
	statuses := map[string]string{}
	for _, item := range items {
		id, _ := item["id"].(string)
		status, _ := item["status"].(string)
		statuses[id] = status
	}
	if statuses["uv-cache"] != string(ItemCleaned) {
		t.Errorf("uv-cache status = %q, want cleaned", statuses["uv-cache"])
	}
	if !strings.HasPrefix(statuses["repo-update-"+testOperationID], string(ItemFailed)) {
		t.Errorf("repo-update status = %q, want failed", statuses["repo-update-"+testOperationID])
	}
	if dirExists(layout.UVCacheDir()) {
		t.Error("uv cache was not cleaned before failure")
	}
	if !dirExists(updateDir) {
		t.Error("failed repo.update directory was removed")
	}
}

// readRootEntries 返回根目录下的条目名列表，用于断言 cleanup 不新增根级目录。
func readRootEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestCleanup_ContextCancellation(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	service, _ := newTestService(t, layout)
	emitter, _ := newTestEmitter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.Run(ctx, emitter)
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeOperationCancelled {
		t.Errorf("error code = %q, want OPERATION_CANCELLED", cleanupErr.Code())
	}
}

// TestCleanup_BuildPlanCancellationPropagatesContextError 证明 buildPlan 的
// 枚举阶段在 ctx 取消时原样传播 context.Canceled，不包装成 40/70。
func TestCleanup_BuildPlanCancellationPropagatesContextError(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.RepoDir(), "app", "main.py"), []byte("x"))
	service, _ := newTestService(t, layout)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("enumerate python caches", func(t *testing.T) {
		items, err := service.enumeratePythonCaches(ctx, testOperationID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("enumeratePythonCaches() error = %v, want context.Canceled", err)
		}
		if items != nil {
			t.Errorf("items = %v, want nil", items)
		}
	})
	t.Run("build plan", func(t *testing.T) {
		plan, err := service.buildPlan(ctx, testOperationID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("buildPlan() error = %v, want context.Canceled", err)
		}
		if plan != nil {
			t.Errorf("plan = %v, want nil", plan)
		}
	})
}

func TestCleanup_ResultDetailsDoNotLeakPaths(t *testing.T) {
	t.Parallel()
	layout, root := newTestLayout(t)
	writeFixtureFile(t, filepath.Join(layout.UVCacheDir(), "cache.bin"), []byte("x"))
	updateDir, err := layout.RepoUpdateDir(testOperationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	writeFixtureFile(t, filepath.Join(updateDir, "partial"), []byte("stale"))
	service, _ := newTestService(t, layout)

	report, err := runService(t, service)
	if err == nil {
		t.Fatal("Run() error = nil, want failure for path-leak fixture")
	}
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("Run() error type = %T, want *Error", err)
	}
	encoded, marshalErr := json.Marshal(cleanupErr.Details())
	if marshalErr != nil {
		t.Fatalf("Marshal(details) error = %v", marshalErr)
	}
	if strings.Contains(string(encoded), root) {
		t.Errorf("error details leak app root path: %s", encoded)
	}
	reportEncoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatalf("Marshal(report) error = %v", marshalErr)
	}
	if strings.Contains(string(reportEncoded), root) {
		t.Errorf("report leaks app root path: %s", reportEncoded)
	}
}

func TestCleanup_NewRejectsMissingInputs(t *testing.T) {
	t.Parallel()
	layout, _ := newTestLayout(t)
	if _, err := New(layout, nil); err == nil {
		t.Error("New() with nil auditor error = nil, want error")
	}
	if _, err := New(nil, &memoryAuditor{}); err == nil {
		t.Error("New() with nil layout error = nil, want error")
	}
}
