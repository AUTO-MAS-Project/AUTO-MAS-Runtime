package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
