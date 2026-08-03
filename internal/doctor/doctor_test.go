package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const testOperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

var testTime = time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)

func testProbes() Probes {
	return Probes{
		UVVersion: func(context.Context, string) (string, error) {
			return "uv 0.8.0", nil
		},
		DiskFree: func(context.Context, string) (uint64, error) {
			return 1 << 40, nil
		},
	}
}

func newTestEmitter(t *testing.T) (*protocol.Emitter, *bytes.Buffer) {
	t.Helper()
	var stdout bytes.Buffer
	output, err := protocol.NewProcessOutput(&stdout)
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter(
		"dev",
		"doctor",
		[]string{},
		protocol.WithOperationID(testOperationID),
		protocol.WithClock(func() time.Time { return testTime }),
	)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	return emitter, &stdout
}

func mustNewService(t *testing.T, layout *config.Layout, probes Probes) *Service {
	t.Helper()
	service, err := New(layout, probes)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	writeFile(t, path, data)
}

func healthyFixture(t *testing.T) (*config.Layout, string) {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeJSONFile(t, layout.RepoVersionFile(), map[string]string{"version": "v5.4.0-beta.1"})
	writeFile(t, filepath.Join(layout.PythonDir(), "python.exe"), nil)
	if err := os.MkdirAll(layout.VenvDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(venv) error = %v", err)
	}
	uvDir, err := layout.UVVersionDir("0.8.0")
	if err != nil {
		t.Fatalf("UVVersionDir() error = %v", err)
	}
	writeFile(t, filepath.Join(uvDir, "uv.exe"), nil)
	if err := os.MkdirAll(layout.LogsDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(logs) error = %v", err)
	}
	for _, file := range []string{
		layout.BackendStateFile(),
		layout.MutationStateFile(),
		layout.UpdateStateFile(),
		layout.EnvironmentStateFile(),
	} {
		writeJSONFile(t, file, map[string]any{})
	}
	return layout, root
}

func runService(t *testing.T, service *Service) Report {
	t.Helper()
	emitter, _ := newTestEmitter(t)
	report, err := service.Run(context.Background(), emitter)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return report
}

func findCheck(t *testing.T, report Report, id string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("report has no check %q: %+v", id, report)
	return Check{}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if entry.IsDir() {
			snapshot[key] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		snapshot[key] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%q) error = %v", root, err)
	}
	return snapshot
}

func TestDoctor_HealthyFixture(t *testing.T) {
	t.Parallel()
	layout, _ := healthyFixture(t)
	service := mustNewService(t, layout, testProbes())
	report := runService(t, service)
	if report.Summary.Total != 9 || report.Summary.OK != 9 {
		t.Fatalf("summary = %+v, want 9/9 ok", report.Summary)
	}
	repo := findCheck(t, report, "repo")
	if repo.Status != StatusOK {
		t.Errorf("repo status = %q, want ok", repo.Status)
	}
	if got := repo.Details["version"]; got != "v5.4.0-beta.1" {
		t.Errorf("repo details.version = %v, want v5.4.0-beta.1", got)
	}
	uv := findCheck(t, report, "uv")
	if got := uv.Details["version"]; got != "uv 0.8.0" {
		t.Errorf("uv details.version = %v, want uv 0.8.0", got)
	}
}

func TestDoctor_MissingFixtureCollectsAllChecks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service := mustNewService(t, layout, testProbes())
	report := runService(t, service)
	if len(report.Checks) != 9 {
		t.Fatalf("check count = %d, want 9", len(report.Checks))
	}
	wantStatuses := map[string]Status{
		"app-root":      StatusOK,
		"layout":        StatusMissing,
		"uv":            StatusMissing,
		"python":        StatusMissing,
		"repo":          StatusMissing,
		"venv":          StatusMissing,
		"runtime-state": StatusMissing,
		"mutex":         StatusOK,
		"disk":          StatusOK,
	}
	for id, want := range wantStatuses {
		if got := findCheck(t, report, id).Status; got != want {
			t.Errorf("check %q status = %q, want %q", id, got, want)
		}
	}
	if report.Summary.Total != 9 || report.Summary.Missing != 6 || report.Summary.Error != 0 {
		t.Errorf("summary = %+v, want 9 total/6 missing/0 error", report.Summary)
	}
}

// TestService_MissingCheckEmitsSkippedProgress 证明 missing 检查项不再伪装成
// succeeded。human 是默认输出模式且 human renderer 不渲染 details，
// result.details.checks 这张唯一的结构化检查表在默认模式下不可见，用户只能
// 依赖 progress 状态；把 missing 渲染成 succeeded 等于对着 6 项缺失报全绿。
func TestService_MissingCheckEmitsSkippedProgress(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service := mustNewService(t, layout, testProbes())
	emitter, stdout := newTestEmitter(t)
	report, err := service.Run(context.Background(), emitter)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	statuses := terminalProgressStatuses(t, stdout.String())
	for _, check := range report.Checks {
		got, ok := statuses[check.Name]
		if !ok {
			t.Errorf("check %q emitted no terminal progress", check.ID)
			continue
		}
		want := map[Status]string{
			StatusOK:      string(protocol.ProgressSucceeded),
			StatusMissing: string(protocol.ProgressSkipped),
			StatusError:   string(protocol.ProgressFailed),
		}[check.Status]
		if got != want {
			t.Errorf(
				"check %q status %q emitted progress %q, want %q",
				check.ID, check.Status, got, want,
			)
		}
	}
	if report.Summary.Missing == 0 {
		t.Fatal("fixture produced no missing checks; test proves nothing")
	}
}

// TestService_EmitsSummaryProgress 证明全部检查项之后追加一条汇总 progress，
// 使 human 模式在不渲染 details 的前提下仍能看到诊断结论。
func TestService_EmitsSummaryProgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fixture    func(t *testing.T) *config.Layout
		wantStatus string
	}{
		{
			name: "healthy",
			fixture: func(t *testing.T) *config.Layout {
				t.Helper()
				layout, _ := healthyFixture(t)
				return layout
			},
			wantStatus: string(protocol.ProgressSucceeded),
		},
		{
			name: "missing",
			fixture: func(t *testing.T) *config.Layout {
				t.Helper()
				root := t.TempDir()
				layout, err := config.NewLayout(root, root)
				if err != nil {
					t.Fatalf("NewLayout() error = %v", err)
				}
				return layout
			},
			wantStatus: string(protocol.ProgressSkipped),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := mustNewService(t, test.fixture(t), testProbes())
			emitter, stdout := newTestEmitter(t)
			report, err := service.Run(context.Background(), emitter)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			summary := lastProgressEvent(t, stdout.String())
			if got := summary["status"]; got != test.wantStatus {
				t.Errorf("summary progress status = %v, want %q", got, test.wantStatus)
			}
			message, _ := summary["message"].(string)
			for _, want := range []string{
				strconv.Itoa(report.Summary.Total),
				strconv.Itoa(report.Summary.OK),
				strconv.Itoa(report.Summary.Missing),
				strconv.Itoa(report.Summary.Error),
			} {
				if !strings.Contains(message, want) {
					t.Errorf("summary message %q lacks count %q", message, want)
				}
			}
		})
	}
}

// terminalProgressStatuses 把 NDJSON 中每个检查项的终态 progress 收集成
// 「检查名 → status」映射；running 事件不计入。
func terminalProgressStatuses(t *testing.T, stdout string) map[string]string {
	t.Helper()
	statuses := make(map[string]string)
	for _, event := range progressEvents(t, stdout) {
		status, _ := event["status"].(string)
		if status == string(protocol.ProgressRunning) {
			continue
		}
		message, _ := event["message"].(string)
		name, _, found := strings.Cut(message, "：")
		if !found {
			continue
		}
		statuses[name] = status
	}
	return statuses
}

// lastProgressEvent 返回最后一条 progress 事件。
func lastProgressEvent(t *testing.T, stdout string) map[string]any {
	t.Helper()
	events := progressEvents(t, stdout)
	if len(events) == 0 {
		t.Fatal("stdout contains no progress events")
	}
	return events[len(events)-1]
}

func progressEvents(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("Unmarshal(%q) error = %v", line, err)
		}
		if event["type"] == "progress" {
			events = append(events, event)
		}
	}
	return events
}

func TestDoctor_PartialFixtureKeepsAllCheckResults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeFile(t, layout.RepoVersionFile(), []byte("not-json"))
	if err := os.MkdirAll(layout.LogsDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(logs) error = %v", err)
	}
	service := mustNewService(t, layout, testProbes())
	report := runService(t, service)
	if len(report.Checks) != 9 {
		t.Fatalf("check count = %d, want 9 (no early termination)", len(report.Checks))
	}
	repo := findCheck(t, report, "repo")
	if repo.Status != StatusError {
		t.Errorf("repo status = %q, want error", repo.Status)
	}
	if findCheck(t, report, "mutex").Status != StatusOK {
		t.Errorf("mutex check missing despite earlier repo error")
	}
	if findCheck(t, report, "disk").Status != StatusOK {
		t.Errorf("disk check missing despite earlier repo error")
	}
}

func TestDoctor_UVProbeInjected(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		layout, _ := healthyFixture(t)
		probes := testProbes()
		probes.UVVersion = func(context.Context, string) (string, error) {
			return "uv 0.9.0", nil
		}
		report := runService(t, mustNewService(t, layout, probes))
		check := findCheck(t, report, "uv")
		if check.Status != StatusOK || check.Details["version"] != "uv 0.9.0" {
			t.Errorf("uv check = %+v, want ok with version uv 0.9.0", check)
		}
	})
	t.Run("probe error", func(t *testing.T) {
		layout, _ := healthyFixture(t)
		probes := testProbes()
		probes.UVVersion = func(context.Context, string) (string, error) {
			return "", errors.New("uv probe failed")
		}
		report := runService(t, mustNewService(t, layout, probes))
		check := findCheck(t, report, "uv")
		if check.Status != StatusError {
			t.Errorf("uv check status = %q, want error", check.Status)
		}
	})
}

func TestDoctor_DiskProbeInjected(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		layout, _ := healthyFixture(t)
		probes := testProbes()
		probes.DiskFree = func(context.Context, string) (uint64, error) {
			return 12345, nil
		}
		report := runService(t, mustNewService(t, layout, probes))
		check := findCheck(t, report, "disk")
		if check.Status != StatusOK || check.Details["freeBytes"] != uint64(12345) {
			t.Errorf("disk check = %+v, want ok with 12345 bytes", check)
		}
	})
	t.Run("probe error", func(t *testing.T) {
		layout, _ := healthyFixture(t)
		probes := testProbes()
		probes.DiskFree = func(context.Context, string) (uint64, error) {
			return 0, errors.New("disk probe failed")
		}
		report := runService(t, mustNewService(t, layout, probes))
		check := findCheck(t, report, "disk")
		if check.Status != StatusError {
			t.Errorf("disk check status = %q, want error", check.Status)
		}
	})
}

func TestDoctor_MutexProbeReportsOccupancy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service := mustNewService(t, layout, testProbes())

	heldSet, err := lock.NewSet(context.Background(), layout)
	if err != nil {
		t.Fatalf("lock.NewSet() error = %v", err)
	}
	acquisition, err := heldSet.AcquireMutation(context.Background())
	if err != nil {
		t.Fatalf("AcquireMutation() error = %v", err)
	}

	report := runService(t, service)
	check := findCheck(t, report, "mutex")
	if check.Status != StatusOK {
		t.Fatalf("mutex check status = %q, want ok", check.Status)
	}
	if got := check.Details["mutation"]; got != true {
		t.Errorf("mutation held = %v, want true", got)
	}
	if got := check.Details["backend"]; got != false {
		t.Errorf("backend held = %v, want false", got)
	}

	if err := acquisition.Lease().Close(); err != nil {
		t.Fatalf("lease.Close() error = %v", err)
	}
	if err := heldSet.Close(); err != nil {
		t.Fatalf("set.Close() error = %v", err)
	}

	report = runService(t, service)
	check = findCheck(t, report, "mutex")
	if got := check.Details["mutation"]; got != false {
		t.Errorf("mutation held after release = %v, want false", got)
	}
}

func TestDoctor_NoPersistentSideEffects(t *testing.T) {
	t.Parallel()
	layout, root := healthyFixture(t)
	service := mustNewService(t, layout, testProbes())
	before := snapshotTree(t, root)
	runService(t, service)
	after := snapshotTree(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("directory tree changed after doctor:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestDoctor_NewRejectsMissingInputs(t *testing.T) {
	t.Parallel()
	layout, _ := healthyFixture(t)
	if _, err := New(layout, Probes{}); err == nil {
		t.Error("New() with empty probes error = nil, want error")
	}
	if _, err := New(nil, testProbes()); err == nil {
		t.Error("New() with nil layout error = nil, want error")
	}
}

func TestDoctor_CheckStatusesAreStable(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusOK, StatusMissing, StatusError} {
		if !status.Valid() {
			t.Errorf("status %q is not valid", status)
		}
	}
	if Status("mystery").Valid() {
		t.Error("unknown status is valid")
	}
}

func TestDoctor_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()
	layout, _ := healthyFixture(t)
	service := mustNewService(t, layout, testProbes())
	emitter, _ := newTestEmitter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Run(ctx, emitter); err == nil {
		t.Fatal("Run() error = nil, want cancelled error")
	}
}

func TestDoctor_ErrorStringDoesNotExposePaths(t *testing.T) {
	t.Parallel()
	serviceErr := NewError(
		protocol.CodeMutexOperationFailed,
		protocol.StageDoctor,
		"锁探测失败",
		map[string]any{},
		errors.New("boom"),
	)
	if strings.Contains(serviceErr.Error(), "C:\\") {
		t.Errorf("Error() exposes path-like text: %q", serviceErr.Error())
	}
	if !errors.Is(serviceErr, serviceErr.Unwrap()) {
		t.Error("errors.Is(serviceErr, cause) = false, want true")
	}
}

// TestDoctor_ResultDetailsDoNotLeakPaths 证明错误检查项与整个 Report 的
// wire 形态不包含任何绝对路径（含 JSON 转义形式），原始错误串只进 stderr/日志。
func TestDoctor_ResultDetailsDoNotLeakPaths(t *testing.T) {
	t.Parallel()
	layout, root := healthyFixture(t)
	probes := testProbes()
	probes.UVVersion = func(context.Context, string) (string, error) {
		return "", errors.New(root + `\runtime\tools\uv\0.8.0\uv.exe: access denied`)
	}
	probes.DiskFree = func(context.Context, string) (uint64, error) {
		return 0, errors.New(root + `\app-root: disk probe failed`)
	}
	service := mustNewService(t, layout, probes)
	report := runService(t, service)

	reportData, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal(report) error = %v", err)
	}
	checksData, err := json.Marshal(report.Checks)
	if err != nil {
		t.Fatalf("Marshal(checks) error = %v", err)
	}
	escapedRoot := strings.ReplaceAll(root, `\`, `\\`)
	for name, data := range map[string]string{
		"report": string(reportData),
		"checks": string(checksData),
	} {
		if strings.Contains(data, escapedRoot) {
			t.Errorf("%s leaks app root path (escaped form): %s", name, data)
		}
		if strings.Contains(data, strings.ReplaceAll(root, `\`, `/`)) {
			t.Errorf("%s leaks app root path (slash form): %s", name, data)
		}
	}
}

// TestReadBounded_EnforcesLimitWithoutStat 证明大小上限由实际读取决定，
// 而不是先 Stat 再 ReadFile 的咨询值：恰好等于上限的文件可读，
// 超出一字节即报 errStateFileTooLarge。
func TestReadBounded_EnforcesLimitWithoutStat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int
		limit   int64
		wantErr bool
	}{
		{name: "below limit", size: 4, limit: 8},
		{name: "exactly at limit", size: 8, limit: 8},
		{name: "one byte over limit", size: 9, limit: 8, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "payload.bin")
			writeFile(t, path, bytes.Repeat([]byte("x"), test.size))
			data, err := readBounded(path, test.limit)
			if test.wantErr {
				if !errors.Is(err, errStateFileTooLarge) {
					t.Fatalf("readBounded() error = %v, want errStateFileTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readBounded() error = %v, want nil", err)
			}
			if len(data) != test.size {
				t.Errorf("len(data) = %d, want %d", len(data), test.size)
			}
		})
	}
}

// TestService_UVCheckHonoursTotalBudget 证明 uv 检查项有整项总预算：
// 多个残留版本目录不会让探测串行叠加成 N 倍单次超时。探针替身在预算耗尽后
// 立刻观察到 ctx 已取消，因此调用次数远小于候选数。
func TestService_UVCheckHonoursTotalBudget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	const candidates = 5
	for i := range candidates {
		uvDir, err := layout.UVVersionDir(fmt.Sprintf("0.8.%d", i))
		if err != nil {
			t.Fatalf("UVVersionDir() error = %v", err)
		}
		writeFile(t, filepath.Join(uvDir, "uv.exe"), nil)
	}

	var calls int
	probes := testProbes()
	probes.UVVersion = func(ctx context.Context, _ string) (string, error) {
		calls++
		// 第一个候选就耗尽整项预算：探针替身直接返回 ctx 的超时错误。
		<-ctx.Done()
		return "", ctx.Err()
	}
	service := mustNewService(t, layout, probes)

	budget, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	check := service.checkUV(budget)

	if check.Status != StatusError {
		t.Errorf("uv check status = %q, want error", check.Status)
	}
	if calls != 1 {
		t.Errorf("UVVersion called %d times, want 1 (budget must stop enumeration)", calls)
	}
}

// TestDoctor_ErrorDetailsUseStableKinds 证明错误检查项的 details.error
// 只使用稳定分类词（not-found/access-denied/invalid/timeout/other）。
func TestDoctor_ErrorDetailsUseStableKinds(t *testing.T) {
	t.Parallel()
	stable := map[string]bool{
		"not-found":     true,
		"access-denied": true,
		"invalid":       true,
		"timeout":       true,
		"other":         true,
	}
	spec := checkSpec{id: "probe", name: "探测"}
	checks := []Check{
		spec.failedBecause("msg", fs.ErrNotExist),
		spec.failedBecause("msg", fs.ErrPermission),
		spec.failedBecause("msg", context.DeadlineExceeded),
		spec.failedBecause("msg", errors.New(`C:\leak`)),
	}
	for _, check := range checks {
		kind, ok := check.Details["error"].(string)
		if !ok {
			t.Fatalf("check %q details.error = %#v, want string", check.ID, check.Details["error"])
		}
		if !stable[kind] {
			t.Errorf("check %q details.error = %q, want stable kind", check.ID, kind)
		}
	}
}
