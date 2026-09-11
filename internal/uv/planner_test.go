package uv

import (
	"context"
	"errors"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// singleSourcePlan 返回只含指定 key 的 Plan，用来证明消费方拿的是注入顺序而不是目录顺序。
func singleSourcePlan(t *testing.T, kind mirror.Kind, key string) mirror.Plan {
	t.Helper()
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	source, ok := catalog.Source(kind, key)
	if !ok {
		t.Fatalf("source %s/%s missing", kind, key)
	}
	only, err := mirror.NewCatalog(append(otherKindSources(t, catalog, kind), source))
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := mirror.BuildPlan(only, policy, kind)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	return plan
}

// otherKindSources 补齐其它 Kind 的源，使单源目录仍能通过 Catalog 校验（每个 Kind 至少一个官方源）。
func otherKindSources(t *testing.T, catalog *mirror.Catalog, except mirror.Kind) []mirror.Source {
	t.Helper()
	sources := make([]mirror.Source, 0, 8)
	for _, kind := range mirror.AllKinds() {
		if kind == except {
			continue
		}
		sources = append(sources, catalog.Sources(kind)...)
	}
	return sources
}

// TestBootstrap_UsesInjectedPlanner 锁定 uv 下载按注入的 PlanFunc 取顺序，注入失败即 bootstrap 失败。
func TestBootstrap_UsesInjectedPlanner(t *testing.T) {
	layout := newUVTestLayout(t)
	artifact := testArtifact("planned archive")
	downloader := &fakeDownloader{payload: []byte("planned archive")}
	checker := &fakeVersionChecker{}
	var seenKinds []mirror.Kind
	planner := mirror.PlanFunc(func(_ context.Context, _ mirror.Policy, kind mirror.Kind) (mirror.Plan, error) {
		seenKinds = append(seenKinds, kind)
		return singleSourcePlan(t, kind, "github"), nil
	})
	bootstrapper, err := NewBootstrapper(layout,
		WithArtifact(artifact),
		WithDownloader(func() *fakeDownloader {
			downloader.path = mustDownloadPath(t, layout, artifact.Name)
			return downloader
		}()),
		WithVersionChecker(checker),
		WithArchiveExtractor(&fakeExtractor{}),
		WithPublisher(&fakePublisher{}),
		WithBootstrapPlanner(planner),
	)
	if err != nil {
		t.Fatalf("NewBootstrapper() error = %v", err)
	}
	if _, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t)); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if len(seenKinds) != 1 || seenKinds[0] != mirror.KindUV {
		t.Fatalf("planner kinds = %v, want [uv]", seenKinds)
	}
	if downloader.request.URL == "" || !containsSubstring(downloader.request.URL, "github.com/astral-sh/uv") {
		t.Fatalf("download url = %q, want the injected github source", downloader.request.URL)
	}

	failing := mirror.PlanFunc(func(context.Context, mirror.Policy, mirror.Kind) (mirror.Plan, error) {
		return mirror.Plan{}, errors.New("planner exploded")
	})
	broken, err := NewBootstrapper(newUVTestLayout(t),
		WithArtifact(artifact),
		WithDownloader(&fakeDownloader{}),
		WithVersionChecker(&fakeVersionChecker{}),
		WithBootstrapPlanner(failing),
	)
	if err != nil {
		t.Fatalf("NewBootstrapper() error = %v", err)
	}
	if _, err := broken.Ensure(t.Context(), testOperationID, testMirrorPolicy(t)); err == nil {
		t.Fatal("Ensure() with failing planner error = nil, want error")
	}
	if _, err := NewBootstrapper(layout, WithBootstrapPlanner(nil)); err == nil {
		t.Fatal("NewBootstrapper(WithBootstrapPlanner(nil)) error = nil, want error")
	}
}

// TestDependencies_UsesInjectedPlanner 锁定依赖同步按注入顺序：只给官方源时直接走原锁 --locked。
func TestDependencies_UsesInjectedPlanner(t *testing.T) {
	fixture := newMirrorSyncFixture(t)
	planner := mirror.PlanFunc(func(_ context.Context, _ mirror.Policy, kind mirror.Kind) (mirror.Plan, error) {
		return singleSourcePlan(t, kind, "pypi"), nil
	})
	service, err := NewDependenciesService(
		fixture.layout,
		fixture.runner,
		&fakeTreeRemover{},
		WithDependenciesStagingRemover(managedTreeRemover{layout: fixture.layout}),
		WithDependenciesPlanner(planner),
	)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	fixture.service = service
	result, err := service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Source != "pypi" || result.LockRewritten || result.AttemptCount != 1 {
		t.Fatalf("result = %+v, want a single official attempt", result)
	}
	if _, err := NewDependenciesService(fixture.layout, fixture.runner, &fakeTreeRemover{}, WithDependenciesPlanner(nil)); err == nil {
		t.Fatal("WithDependenciesPlanner(nil) error = nil, want error")
	}
}

// TestPython_UsesInjectedPlanner 锁定 Python 安装的镜像顺序来自注入的 PlanFunc。
func TestPython_UsesInjectedPlanner(t *testing.T) {
	t.Parallel()

	layout := newUVTestLayout(t)
	projectDir := t.TempDir()
	runner := &fakePythonRunner{
		listOutput:     `[{"version":"3.12.10"}]`,
		findOutput:     "C:/runtime/python/3.12.10/python.exe",
		installResults: []fakeRunnerResponse{{result: UVResult{ExitCode: 0}}},
	}
	planner := mirror.PlanFunc(func(_ context.Context, _ mirror.Policy, kind mirror.Kind) (mirror.Plan, error) {
		return singleSourcePlan(t, kind, "github"), nil
	})
	service, err := NewPythonService(layout, runner, WithPythonPlanner(planner))
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}
	writePythonProject(t, projectDir, "3.12.10", "[project]\nname = \"auto-mas\"\nrequires-python = \">=3.12,<3.13\"\n")
	if _, err := service.Prepare(t.Context(), PythonRequest{ProjectDir: projectDir, MirrorPolicy: testMirrorPolicy(t)}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	var installEnv string
	for _, call := range runner.calls {
		if call.args[1] == "install" {
			installEnv = call.options.Environment[uvPythonInstallMirrorEnv]
		}
	}
	if !containsSubstring(installEnv, "github.com/astral-sh/python-build-standalone") {
		t.Fatalf("install mirror = %q, want the injected github source", installEnv)
	}
	if _, err := NewPythonService(layout, runner, WithPythonPlanner(nil)); err == nil {
		t.Fatal("WithPythonPlanner(nil) error = nil, want error")
	}
}

func containsSubstring(value, substring string) bool {
	return len(substring) > 0 && len(value) >= len(substring) && (func() bool {
		for index := 0; index+len(substring) <= len(value); index++ {
			if value[index:index+len(substring)] == substring {
				return true
			}
		}
		return false
	})()
}

// TestProductionEnvironment_PlannerOptionValidation 锁定生产适配器只接受非空 planner，且不传时构造照旧。
func TestProductionEnvironment_PlannerOptionValidation(t *testing.T) {
	t.Parallel()

	layout := newUVTestLayout(t)
	if _, err := NewProductionEnvironment(layout); err != nil {
		t.Fatalf("NewProductionEnvironment() error = %v", err)
	}
	planner := mirror.CatalogPlanFunc(mustDefaultCatalog(t))
	environment, err := NewProductionEnvironment(layout, WithProductionPlanner(planner))
	if err != nil {
		t.Fatalf("NewProductionEnvironment(planner) error = %v", err)
	}
	if environment.plan == nil || environment.bootstrap.plan == nil {
		t.Fatal("planner was not propagated to the production environment and bootstrapper")
	}
	if _, err := NewProductionEnvironment(layout, WithProductionPlanner(nil)); err == nil {
		t.Fatal("WithProductionPlanner(nil) error = nil, want error")
	}
	if _, err := NewProductionEnvironment(layout, nil); err == nil {
		t.Fatal("NewProductionEnvironment(nil option) error = nil, want error")
	}
}

func mustDefaultCatalog(t *testing.T) *mirror.Catalog {
	t.Helper()
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	return catalog
}
