package uv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// fakeRelaySession 记录中继会话看到的配置，并按预设返回摘要；不监听任何端口。
type fakeRelaySession struct {
	config   relay.Config
	items    []relay.Item
	summary  relay.Summary
	closed   int
	baseURL  string
	progress func(relay.Progress) error
}

func (f *fakeRelaySession) BaseURL() string { return f.baseURL }

func (f *fakeRelaySession) Register(items []relay.Item) { f.items = append(f.items, items...) }

func (f *fakeRelaySession) Summary() relay.Summary { return f.summary }

func (f *fakeRelaySession) Close() error {
	f.closed++
	return nil
}

// fakeRelayFactory 返回预置会话或启动错误，并记录 Deps 以便测试驱动进度回调。
type fakeRelayFactory struct {
	session *fakeRelaySession
	err     error
	starts  int
}

func (f *fakeRelayFactory) start(_ context.Context, cfg relay.Config, deps relay.Deps) (relaySession, error) {
	f.starts++
	if f.err != nil {
		return nil, f.err
	}
	f.session.config = cfg
	f.session.progress = deps.Progress
	return f.session, nil
}

func newRelayFixture(t *testing.T, factory *fakeRelayFactory) *mirrorSyncFixture {
	t.Helper()
	fixture := newMirrorSyncFixture(t)
	service, err := NewDependenciesService(
		fixture.layout,
		fixture.runner,
		&fakeTreeRemover{},
		WithDependenciesStagingRemover(managedTreeRemover{layout: fixture.layout}),
		WithDependenciesRelay(factory.start),
	)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	fixture.service = service
	return fixture
}

// TestDependencies_SyncUsesRelayAndSingleAttempt 锁定增补 2 C17 第 2 条：在线同步只跑一次经中继的
// uv sync --frozen，锁副本前缀指向回环地址，已知制品登记给中继，uv 环境带 UV_HTTP_TIMEOUT。
func TestDependencies_SyncUsesRelayAndSingleAttempt(t *testing.T) {
	session := &fakeRelaySession{
		baseURL: "http://127.0.0.1:39170",
		summary: relay.Summary{Files: 1, Bytes: 166393, BySource: map[string]int64{"tsinghua": 166393}},
	}
	factory := &fakeRelayFactory{session: session}
	fixture := newRelayFixture(t, factory)
	staging := fixture.stagingDir(t)
	var observedLock string
	fixture.runner.onRun = func(args []string, _ RunOptions) {
		if len(args) < 3 || args[1] != "--project" || args[2] != staging {
			return
		}
		lock, err := os.ReadFile(filepath.Join(staging, "uv.lock"))
		if err != nil {
			t.Errorf("ReadFile(staging uv.lock) error = %v", err)
			return
		}
		observedLock = string(lock)
	}

	result, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if factory.starts != 1 || session.closed != 1 {
		t.Fatalf("relay starts = %d, closes = %d, want 1/1", factory.starts, session.closed)
	}
	if got, want := len(fixture.runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want lock check + one relay sync", got)
	}
	sync := fixture.runner.calls[1]
	if !reflect.DeepEqual(sync.args, mirrorSyncArgsFor(staging)) {
		t.Fatalf("sync args = %#v, want %#v", sync.args, mirrorSyncArgsFor(staging))
	}
	if got := sync.options.Environment[uvHTTPTimeoutEnv]; got != relayUVHTTPTimeoutSeconds {
		t.Fatalf("UV_HTTP_TIMEOUT = %q, want %s", got, relayUVHTTPTimeoutSeconds)
	}
	if !containsAll(observedLock, "http://127.0.0.1:39170/simple", "http://127.0.0.1:39170/packages/38/fc/") ||
		strings.Contains(observedLock, "files.pythonhosted.org") {
		t.Fatalf("staging lock not rewritten to the relay: %q", observedLock)
	}
	if len(session.config.Items) != 1 || session.config.Items[0].Path != "38/fc/certifi-2025.1.31-py3-none-any.whl" ||
		session.config.Items[0].Size != 166393 || session.config.Items[0].SHA256 != "ca78db4565a652026a4db2bcdf68f2fb589ea80d0be70e03929ed730746b84fe" {
		t.Fatalf("registered items = %+v, want the certifi wheel", session.config.Items)
	}
	packages := session.config.Upstreams[relay.RoutePackages]
	if len(packages) != 4 || packages[0].Key != "aliyun" || packages[3].Key != "pypi" ||
		packages[3].Base != "https://files.pythonhosted.org/packages/" {
		t.Fatalf("packages upstreams = %+v, want catalog order ending with pypi", packages)
	}
	simple := session.config.Upstreams[relay.RouteSimple]
	if len(simple) != 4 || simple[0].Base != "https://mirrors.aliyun.com/pypi/simple/" ||
		simple[0].RewriteFrom != "https://mirrors.aliyun.com/pypi/packages/" {
		t.Fatalf("simple upstreams = %+v", simple)
	}
	if session.config.StagingDir != fixture.layout.RelayStagingDir() {
		t.Fatalf("staging dir = %q, want %q", session.config.StagingDir, fixture.layout.RelayStagingDir())
	}
	if result.Source != "tsinghua" || result.AttemptCount != 1 || !result.LockRewritten || !result.Synchronized ||
		result.Relay == nil || result.Relay.Files != 1 {
		t.Fatalf("result = %+v, want a single relayed attempt attributed to tsinghua", result)
	}
	if len(fixture.attempts) != 1 || fixture.attempts[0].Source != "relay" || fixture.attempts[0].Fallback {
		t.Fatalf("attempts = %+v, want one relay attempt", fixture.attempts)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

// TestDependencies_RelayStartFailureFallsBackToMirrors 锁定中继起不来时退回 T13.4 的逐源轮换。
func TestDependencies_RelayStartFailureFallsBackToMirrors(t *testing.T) {
	factory := &fakeRelayFactory{err: errors.New("bind: address in use")}
	fixture := newRelayFixture(t, factory)
	fixture.runner.responses = []fakeRunnerResponse{{}, {}}

	result, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if factory.starts != 1 {
		t.Fatalf("relay starts = %d, want 1", factory.starts)
	}
	if result.Source != "aliyun" || result.AttemptCount != 1 || !result.LockRewritten || result.Relay != nil {
		t.Fatalf("result = %+v, want the first mirror via the legacy rotation", result)
	}
	if len(fixture.attempts) != 1 || fixture.attempts[0].Source != "aliyun" {
		t.Fatalf("attempts = %+v, want legacy aliyun attempt", fixture.attempts)
	}
}

// TestDependencies_RelayFailuresMapToMirrorExhausted 锁定中继报告了文件失败时 uv 失败映射为 MIRROR_EXHAUSTED，
// details 带 relay 摘要；没有文件失败的 uv 失败仍按既有依赖同步失败映射。
func TestDependencies_RelayFailuresMapToMirrorExhausted(t *testing.T) {
	session := &fakeRelaySession{
		baseURL: "http://127.0.0.1:39170",
		summary: relay.Summary{Failures: []relay.FileFailure{{
			Item:     "certifi-2025.1.31-py3-none-any.whl",
			Attempts: []relay.AttemptOutcome{{Source: "aliyun", Outcome: relay.OutcomeHTTPStatus}, {Source: "pypi", Outcome: relay.OutcomeNetwork}},
		}}},
	}
	fixture := newRelayFixture(t, &fakeRelayFactory{session: session})
	fixture.runner.responses = []fakeRunnerResponse{{}, {result: UVResult{ExitCode: 1}, err: errors.New("uv sync failed")}}

	_, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	var structured *Error
	if !errors.As(err, &structured) || structured.Code() != protocol.CodeMirrorExhausted {
		t.Fatalf("Sync() error = %v, want MIRROR_EXHAUSTED", err)
	}
	details := structured.Details()
	relayDetails, ok := details["relay"].(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want relay summary", details)
	}
	failures, ok := relayDetails["failures"].([]map[string]any)
	if !ok || len(failures) != 1 || failures[0]["item"] != "certifi-2025.1.31-py3-none-any.whl" {
		t.Fatalf("relay failures = %#v", relayDetails["failures"])
	}
	if session.closed != 1 {
		t.Fatalf("relay closes = %d, want 1", session.closed)
	}
	fixture.assertNoStagingLeftovers(t)

	clean := &fakeRelaySession{baseURL: "http://127.0.0.1:39171"}
	plain := newRelayFixture(t, &fakeRelayFactory{session: clean})
	plain.runner.responses = []fakeRunnerResponse{{}, {result: UVResult{ExitCode: 2}, err: errors.New("build failed")}}
	_, err = plain.service.Sync(t.Context(), plain.request(t, mirror.PolicySpec{}))
	if !errors.As(err, &structured) || structured.Code() != protocol.CodeDependencySyncFailed {
		t.Fatalf("Sync() error = %v, want DEPENDENCY_SYNC_FAILED without relay failures", err)
	}
}

// TestDependencies_MirrorOnlySkipsDirectFallback 锁定 --mirror-only 下中继上游不含官方源，且中继失败不直连。
func TestDependencies_MirrorOnlySkipsDirectFallback(t *testing.T) {
	session := &fakeRelaySession{baseURL: "http://127.0.0.1:39170", summary: relay.Summary{Failures: []relay.FileFailure{{Item: "x.whl"}}}}
	fixture := newRelayFixture(t, &fakeRelayFactory{session: session})
	fixture.runner.responses = []fakeRunnerResponse{{}, {result: UVResult{ExitCode: 1}, err: errors.New("uv sync failed")}}

	_, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{MirrorOnly: true}))
	var structured *Error
	if !errors.As(err, &structured) || structured.Code() != protocol.CodeMirrorExhausted {
		t.Fatalf("Sync() error = %v, want MIRROR_EXHAUSTED", err)
	}
	for _, upstream := range session.config.Upstreams[relay.RoutePackages] {
		if upstream.Key == "pypi" {
			t.Fatalf("mirror-only upstreams include the official source: %+v", session.config.Upstreams)
		}
	}
	if got := len(fixture.runner.calls); got != 2 {
		t.Fatalf("runner calls = %d, want no direct --locked fallback", got)
	}
}

// TestDependencies_ProgressCarriesItemAndSource 锁定中继进度原样到达请求方回调。
func TestDependencies_ProgressCarriesItemAndSource(t *testing.T) {
	session := &fakeRelaySession{baseURL: "http://127.0.0.1:39170"}
	fixture := newRelayFixture(t, &fakeRelayFactory{session: session})
	var seen []relay.Progress
	request := fixture.request(t, mirror.PolicySpec{})
	request.Progress = func(progress relay.Progress) error {
		seen = append(seen, progress)
		return nil
	}
	fixture.runner.onRun = func(args []string, _ RunOptions) {
		if len(args) > 1 && args[0] == "sync" && args[1] == "--project" && session.progress != nil {
			_ = session.progress(relay.Progress{Route: relay.RoutePackages, Item: "certifi-2025.1.31-py3-none-any.whl", Source: "aliyun", Received: 10, Total: 166393, BytesPerSecond: 5})
		}
	}
	if _, err := fixture.service.Sync(t.Context(), request); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(seen) != 1 || seen[0].Item != "certifi-2025.1.31-py3-none-any.whl" || seen[0].Source != "aliyun" || seen[0].Total != 166393 {
		t.Fatalf("progress = %+v", seen)
	}
}

// TestDependencies_OfflineNeverStartsRelay 锁定 --offline 完全不起中继、不改写锁。
func TestDependencies_OfflineNeverStartsRelay(t *testing.T) {
	factory := &fakeRelayFactory{session: &fakeRelaySession{baseURL: "http://127.0.0.1:39170"}}
	fixture := newRelayFixture(t, factory)
	if _, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{Offline: true})); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if factory.starts != 0 {
		t.Fatalf("relay starts = %d, want 0 offline", factory.starts)
	}
}
