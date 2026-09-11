package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// fakeRelaySession 记录监督器交给中继的配置与顺序更新，不监听端口。
type fakeRelaySession struct {
	mu       sync.Mutex // 保护 updates 与 closed
	baseURL  string
	config   relay.Config
	updates  map[relay.Route][]relay.Upstream
	closed   int
	updated  chan struct{}
	closeErr error
}

func (f *fakeRelaySession) BaseURL() string { return f.baseURL }

func (f *fakeRelaySession) SetUpstreams(route relay.Route, upstreams []relay.Upstream) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updates == nil {
		f.updates = make(map[relay.Route][]relay.Upstream)
	}
	f.updates[route] = append([]relay.Upstream(nil), upstreams...)
	if f.updated != nil && len(f.updates) == 2 {
		close(f.updated)
		f.updated = nil
	}
	return nil
}

func (f *fakeRelaySession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func fakeRelayStarter(session *fakeRelaySession, err error) (RelayStarter, *int) {
	starts := 0
	return func(_ context.Context, cfg relay.Config, _ relay.Deps) (RelaySession, error) {
		starts++
		if err != nil {
			return nil, err
		}
		session.config = cfg
		return session, nil
	}, &starts
}

// TestBackendSupervise_InjectsRelayAsFirstMirror 锁定增补 2 C17 第 8 条：中继起来后两份列表以回环首项开头，
// 三条路由的上游按目录顺序交给中继，监督结束时中继被关闭。
func TestBackendSupervise_InjectsRelayAsFirstMirror(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.checkErr = nil
	f.proc.keepAlive = true
	session := &fakeRelaySession{baseURL: "http://127.0.0.1:39170"}
	starter, starts := fakeRelayStarter(session, nil)
	f.relay = starter

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}

	if *starts != 1 || session.closed != 1 {
		t.Fatalf("relay starts = %d, closes = %d, want 1/1", *starts, session.closed)
	}
	infrastructure := f.uv.options.Infrastructure
	if infrastructure.RelayBaseURL != "http://127.0.0.1:39170" {
		t.Fatalf("RelayBaseURL = %q", infrastructure.RelayBaseURL)
	}
	if len(infrastructure.PackageIndexSources) == 0 || infrastructure.PackageIndexSources[0] != "https://mirrors.aliyun.com/pypi/simple/" {
		t.Fatalf("package index sources = %v, want catalog order after the relay entry", infrastructure.PackageIndexSources)
	}
	for _, route := range []relay.Route{relay.RoutePackages, relay.RouteSimple, relay.RoutePython} {
		if len(session.config.Upstreams[route]) == 0 {
			t.Errorf("relay config has no upstreams for %s", route)
		}
	}
	if simple := session.config.Upstreams[relay.RouteSimple]; simple[0].RewriteFrom != "https://mirrors.aliyun.com/pypi/packages/" {
		t.Errorf("simple upstream rewrite = %+v", simple[0])
	}
	if !strings.HasSuffix(session.config.StagingDir, `\downloads\relay`) {
		t.Errorf("relay staging dir = %q", session.config.StagingDir)
	}
}

// TestBackendSupervise_RelayStartFailureUsesPlainLists 锁定中继起不来时按今天的直连列表注入且监督照常。
func TestBackendSupervise_RelayStartFailureUsesPlainLists(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.checkErr = nil
	f.proc.keepAlive = true
	starter, starts := fakeRelayStarter(nil, errors.New("bind failed"))
	f.relay = starter

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	if *starts != 1 {
		t.Fatalf("relay starts = %d, want 1", *starts)
	}
	if got := f.uv.options.Infrastructure.RelayBaseURL; got != "" {
		t.Fatalf("RelayBaseURL = %q, want empty without a relay", got)
	}
	if got := f.uv.options.Infrastructure.PackageIndexSources[0]; got != "https://mirrors.aliyun.com/pypi/simple/" {
		t.Fatalf("first package index source = %q, want plain catalog order", got)
	}
}

// TestBackendSupervise_RankerUpdatesRelayOrder 锁定后台测速完成后中继的 packages / simple 上游被替换成实测顺序，
// 且监督结束前等待该 goroutine 退出。
func TestBackendSupervise_RankerUpdatesRelayOrder(t *testing.T) {
	f := newBackendFixture(t)
	f.uv.checkErr = nil
	f.proc.keepAlive = true
	updated := make(chan struct{})
	session := &fakeRelaySession{baseURL: "http://127.0.0.1:39170", updated: updated}
	starter, _ := fakeRelayStarter(session, nil)
	f.relay = starter
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	speeds := map[string]int64{"pypi": 9_000_000, "tsinghua": 3_000_000, "aliyun": 100, "ustc": 50}
	ranker, err := mirror.NewRanker(catalog, mirror.WithRankerProbeFunc(func(_ context.Context, plan mirror.Plan, _ mirror.ProbeTarget, report func(mirror.ProbeResult)) ([]mirror.ProbeResult, error) {
		results := make([]mirror.ProbeResult, 0, len(plan.Sources()))
		for _, source := range plan.Sources() {
			result := mirror.ProbeResult{Source: source, OK: true, BytesPerSecond: speeds[source.Key()], AcceptRanges: true}
			if report != nil {
				report(result)
			}
			results = append(results, result)
		}
		return results, nil
	}))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	f.ranker = ranker
	writeBackendTestLock(t, f)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, f.request()) }()
	waitFor(t, f.emitter.running)
	waitFor(t, updated)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	packages := session.updates[relay.RoutePackages]
	if len(packages) != 4 || packages[0].Key != "pypi" || packages[1].Key != "tsinghua" {
		t.Fatalf("ranked packages upstreams = %+v, want pypi first", packages)
	}
	if session.closed != 1 {
		t.Fatalf("relay closes = %d, want 1", session.closed)
	}
}

// writeBackendTestLock 在受管仓库放一份最小锁，让后台测速有包索引探针目标。
func writeBackendTestLock(t *testing.T, f *backendFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.layout.UVLockFile()), 0o700); err != nil {
		t.Fatalf("MkdirAll(repo) error = %v", err)
	}
	if err := os.WriteFile(f.layout.UVLockFile(), []byte(backendTestLock), 0o600); err != nil {
		t.Fatalf("WriteFile(uv.lock) error = %v", err)
	}
}

const backendTestLock = `version = 1
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
    { url = "https://files.pythonhosted.org/packages/38/fc/certifi-2025.1.31-py3-none-any.whl", hash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", size = 166393 },
]
`
