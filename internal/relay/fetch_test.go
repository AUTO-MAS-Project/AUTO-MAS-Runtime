package relay

import (
	"bytes"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

const wheelPath = "packages/ab/cd/0123/demo-1.0-py3-none-any.whl"

// fakeTimer 是只供失速测试使用的定时器：Stop 从不吞掉已触发信号，触发后由读循环自己消费。
type fakeTimer struct {
	c      chan time.Time
	resets chan time.Duration
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool { return true }

func (t *fakeTimer) Reset(delay time.Duration) bool {
	select {
	case t.resets <- delay:
	default:
	}
	return true
}

func (t *fakeTimer) fire() {
	select {
	case t.c <- time.Time{}:
	default:
	}
}

type fakeTimers struct {
	stall   time.Duration
	mu      sync.Mutex
	created chan *fakeTimer
}

func newFakeTimers(stall time.Duration) *fakeTimers {
	return &fakeTimers{stall: stall, created: make(chan *fakeTimer, 16)}
}

// factory 只替换失速定时器；连接超时仍用真实定时器，两者在测试里必须配置为不同时长。
func (f *fakeTimers) factory(delay time.Duration) timer {
	if delay != f.stall {
		return newRuntimeTimer(delay)
	}
	created := &fakeTimer{
		c:      make(chan time.Time, 1),
		resets: make(chan time.Duration, 64),
	}
	f.mu.Lock()
	select {
	case f.created <- created:
	default:
	}
	f.mu.Unlock()
	return created
}

func (f *fakeTimers) next(t *testing.T) *fakeTimer {
	t.Helper()
	select {
	case created := <-f.created:
		return created
	case <-time.After(testWait):
		t.Fatal("timed out waiting for stall timer creation")
		return nil
	}
}

func packagesConfig(items []Item, mirrors ...*fakeMirror) Config {
	upstreams := make([]Upstream, 0, len(mirrors))
	for i, mirror := range mirrors {
		upstreams = append(upstreams, packagesUpstream(mirror, string(rune('a'+i))))
	}
	return Config{
		Upstreams: map[Route][]Upstream{RoutePackages: upstreams},
		Items:     items,
	}
}

func relayPath(mirrorPath string) string { return "/" + mirrorPath }

func TestFetch_SingleStreamFallsBackOnError(t *testing.T) {
	content := testContent(200_000, 1)
	broken := newFakeMirror(t, false)
	broken.put(wheelPath, content)
	broken.behave(wheelPath, mirrorBehavior{cutAt: 50_000})
	healthy := newFakeMirror(t, false)
	healthy.put(wheelPath, content)
	fixture := startFixture(t, fixtureOptions{
		cfg: packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, broken, healthy),
	}, broken, healthy)

	response, body := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(body), len(content))
	}
	if got := broken.count(wheelPath); got != 1 {
		t.Errorf("broken mirror requests = %d, want 1", got)
	}
	if got := healthy.count(wheelPath); got != 1 {
		t.Errorf("healthy mirror requests = %d, want 1", got)
	}
	if got := fixture.logs.outcomes("a"); len(got) != 1 || got[0] != OutcomeNetwork {
		t.Errorf("outcomes for a = %v, want [network]", got)
	}
	summary := fixture.server.Summary()
	if summary.Files != 1 || summary.Bytes != int64(len(content)) {
		t.Errorf("summary = %+v, want 1 file of %d bytes", summary, len(content))
	}
	if summary.BySource["b"] != int64(len(content)) || summary.BySource["a"] != 0 {
		t.Errorf("bySource = %v, want all bytes from b", summary.BySource)
	}
	if len(summary.Failures) != 0 {
		t.Errorf("failures = %v, want none", summary.Failures)
	}
}

func TestFetch_ChecksumMismatchSwitchesSource(t *testing.T) {
	paths := []string{
		"packages/aa/one-1.0-py3-none-any.whl",
		"packages/bb/two-1.0-py3-none-any.whl",
		"packages/cc/three-1.0-py3-none-any.whl",
	}
	corrupting := newFakeMirror(t, false)
	healthy := newFakeMirror(t, false)
	items := make([]Item, 0, len(paths))
	contents := make(map[string][]byte, len(paths))
	for i, path := range paths {
		content := testContent(4096, byte(i+10))
		contents[path] = content
		corrupting.put(path, content)
		corrupting.behave(path, mirrorBehavior{corrupt: true})
		healthy.put(path, content)
		items = append(items, itemFor(path[len("packages/"):], content))
	}
	fixture := startFixture(t, fixtureOptions{cfg: packagesConfig(items, corrupting, healthy)}, corrupting, healthy)

	for _, path := range paths {
		response, body := fixture.get(t, relayPath(path))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, response.StatusCode)
		}
		if !bytes.Equal(body, contents[path]) {
			t.Fatalf("GET %s body mismatch", path)
		}
	}
	if got := corrupting.count(paths[0]); got != 1 {
		t.Errorf("first file requests on corrupting mirror = %d, want 1", got)
	}
	if got := corrupting.count(paths[1]); got != 1 {
		t.Errorf("second file requests on corrupting mirror = %d, want 1", got)
	}
	if got := corrupting.count(paths[2]); got != 0 {
		t.Errorf("third file requests on corrupting mirror = %d, want 0 after eviction", got)
	}
	if got := fixture.logs.outcomes("a"); len(got) != 2 || got[0] != OutcomeChecksumMismatch || got[1] != OutcomeChecksumMismatch {
		t.Errorf("outcomes for a = %v, want two checksum_mismatch", got)
	}
	summary := fixture.server.Summary()
	if summary.Files != 3 || len(summary.Failures) != 0 {
		t.Errorf("summary = %+v, want 3 files and no failures", summary)
	}
}

func TestFetch_AllSourcesFailedIs502(t *testing.T) {
	content := testContent(64_000, 3)
	erroring := newFakeMirror(t, false)
	erroring.put(wheelPath, content)
	erroring.behave(wheelPath, mirrorBehavior{status: http.StatusInternalServerError})
	cutting := newFakeMirror(t, false)
	cutting.put(wheelPath, content)
	cutting.behave(wheelPath, mirrorBehavior{cutAt: 1000})
	fixture := startFixture(t, fixtureOptions{
		cfg: packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, erroring, cutting),
	}, erroring, cutting)

	response, _ := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	summary := fixture.server.Summary()
	if summary.Files != 0 || summary.Bytes != 0 {
		t.Errorf("summary files/bytes = %d/%d, want 0/0", summary.Files, summary.Bytes)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("failures = %v, want exactly one", summary.Failures)
	}
	failure := summary.Failures[0]
	if failure.Item != "demo-1.0-py3-none-any.whl" {
		t.Errorf("failure item = %q, want file name", failure.Item)
	}
	want := []AttemptOutcome{{Source: "a", Outcome: OutcomeHTTPStatus}, {Source: "b", Outcome: OutcomeNetwork}}
	if len(failure.Attempts) != len(want) {
		t.Fatalf("attempts = %v, want %v", failure.Attempts, want)
	}
	for i := range want {
		if failure.Attempts[i] != want[i] {
			t.Errorf("attempt %d = %v, want %v", i, failure.Attempts[i], want[i])
		}
	}

	// 失败不缓存：修好上游后同一路径可以重试成功。
	erroring.behave(wheelPath, mirrorBehavior{})
	response, body := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("retry status = %d, want 200 with full content", response.StatusCode)
	}
}

func TestFetch_StallTimeoutSwitchesSource(t *testing.T) {
	content := testContent(300_000, 4)
	stalling := newFakeMirror(t, false)
	stalling.put(wheelPath, content)
	stalling.behave(wheelPath, mirrorBehavior{hang: true, hangAt: 20_000})
	healthy := newFakeMirror(t, false)
	healthy.put(wheelPath, content)
	timers := newFakeTimers(15 * time.Second)
	cfg := packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, stalling, healthy)
	cfg.StallTimeout = 15 * time.Second
	cfg.ConnectTimeout = 7 * time.Second
	fixture := startFixture(t, fixtureOptions{
		cfg:       cfg,
		internals: internals{timers: timers.factory},
	}, stalling, healthy)

	results := fixture.getAsync(relayPath(wheelPath))
	stallTimer := timers.next(t)
	select {
	case <-stallTimer.resets:
	case <-time.After(testWait):
		t.Fatal("timed out waiting for the read loop to consume the partial body")
	}
	stallTimer.fire()

	result := awaitResult(t, results, "relayed response")
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("result = %+v, want 200", result)
	}
	if !bytes.Equal(result.body, content) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(result.body), len(content))
	}
	if got := fixture.logs.outcomes("a"); len(got) != 1 || got[0] != OutcomeReadTimeout {
		t.Errorf("outcomes for a = %v, want [read_timeout]", got)
	}
	if got := healthy.count(wheelPath); got != 1 {
		t.Errorf("healthy mirror requests = %d, want 1", got)
	}
}

func TestFetch_UnknownFileProbesSizeAndSkipsChecksum(t *testing.T) {
	content := testContent(50_000, 5)
	const pythonPath = "python/20260807/cpython-3.12.13%2B20260807-x86_64-pc-windows-msvc-install_only_stripped.tar.gz"
	const sidecar = "packages/ab/cd/0123/demo-1.0-py3-none-any.whl.metadata"
	mirror := newFakeMirror(t, true)
	mirror.put(pythonPath, content)
	mirror.put(sidecar, []byte("Metadata-Version: 2.1\n"))
	fixture := startFixture(t, fixtureOptions{cfg: Config{Upstreams: map[Route][]Upstream{
		RoutePackages: {packagesUpstream(mirror, "a")},
		RoutePython:   {pythonUpstream(mirror, "a")},
	}}}, mirror)

	response, body := fixture.get(t, relayPath(pythonPath))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("python status = %d, body %d bytes; want 200 with %d bytes", response.StatusCode, len(body), len(content))
	}
	response, body = fixture.get(t, relayPath(sidecar))
	if response.StatusCode != http.StatusOK || string(body) != "Metadata-Version: 2.1\n" {
		t.Fatalf("sidecar status = %d body %q", response.StatusCode, body)
	}
	recorded := mirror.recorded()
	var methods []string
	for _, request := range recorded {
		if request.path == pythonPath {
			methods = append(methods, request.method)
		}
	}
	if len(methods) != 2 || methods[0] != http.MethodHead || methods[1] != http.MethodGet {
		t.Errorf("python upstream methods = %v, want [HEAD GET] with percent-encoding preserved", methods)
	}
	head, _ := fixture.do(t, http.MethodHead, relayPath(pythonPath), nil)
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(content)) {
		t.Errorf("HEAD status/length = %d/%d, want 200/%d", head.StatusCode, head.ContentLength, len(content))
	}
	partial, part := fixture.do(t, http.MethodGet, relayPath(pythonPath), map[string]string{"Range": "bytes=10-19"})
	if partial.StatusCode != http.StatusPartialContent || !bytes.Equal(part, content[10:20]) {
		t.Errorf("Range status = %d len %d, want 206 with 10 bytes", partial.StatusCode, len(part))
	}
	if got := mirror.count(pythonPath); got != 2 {
		t.Errorf("python upstream requests = %d, want 2 (HEAD + GET, later requests served from staging)", got)
	}
}

func TestFetch_ProgressFailureStopsRelay(t *testing.T) {
	content := testContent(40_000, 6)
	mirror := newFakeMirror(t, false)
	mirror.put(wheelPath, content)
	recorder := &progressRecorder{failErr: errors.New("output closed")}
	fixture := startFixture(t, fixtureOptions{
		cfg:  packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, mirror),
		deps: Deps{Progress: recorder.record},
	}, mirror)

	response, _ := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after progress failure", response.StatusCode)
	}
	response, _ = fixture.get(t, "/packages/xx/other.whl")
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("later status = %d, want 502 once relay is stopped", response.StatusCode)
	}
	if got := mirror.countAll(); got != 1 {
		t.Errorf("upstream requests = %d, want 1 (stopped relay must not fetch)", got)
	}
}
