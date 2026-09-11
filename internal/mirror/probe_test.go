package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const probeTestWindow int64 = 64 * 1024

// fakeProbeClock 是可被多个 goroutine 读取与推进的虚拟时钟。
type fakeProbeClock struct {
	mu  sync.Mutex // 保护 now
	now time.Time
}

func newFakeProbeClock() *fakeProbeClock {
	return &fakeProbeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeProbeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeProbeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

// throttledBody 在每次读到字节后按速率推进虚拟时钟，用来模拟带宽而不 sleep。
type throttledBody struct {
	io.ReadCloser
	clock *fakeProbeClock
	rate  int64
}

func (b *throttledBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.clock.Advance(time.Duration(int64(n) * int64(time.Second) / b.rate))
	}
	return n, err
}

// countingBody 记录响应体是否已关闭，用来证明探针不泄漏连接。
type countingBody struct {
	io.ReadCloser
	open *atomic.Int32
	once sync.Once
}

func (b *countingBody) Close() error {
	b.once.Do(func() { b.open.Add(-1) })
	return b.ReadCloser.Close()
}

type probeServerConfig struct {
	gate         <-chan struct{}
	size         int64
	status       int
	acceptRanges bool
	trickle      int64
	stallHeaders bool
}

// probeServer 返回按配置行为的 TLS 测试服务器，并校验探针请求头。
func probeServer(t *testing.T, cfg probeServerConfig) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		if cfg.gate != nil {
			select {
			case <-cfg.gate:
			case <-request.Context().Done():
				return
			}
		}
		if cfg.stallHeaders {
			<-request.Context().Done()
			return
		}
		if cfg.status != 0 && cfg.status != http.StatusOK && cfg.status != http.StatusPartialContent {
			http.Error(writer, "probe failure", cfg.status)
			return
		}
		if cfg.acceptRanges {
			writer.Header().Set("Accept-Ranges", "bytes")
		}
		writer.Header().Set("Content-Length", strconv.FormatInt(cfg.size, 10))
		if cfg.status == http.StatusPartialContent {
			writer.Header().Set(
				"Content-Range",
				fmt.Sprintf("bytes 0-%d/%d", cfg.size-1, cfg.size*16),
			)
			writer.WriteHeader(http.StatusPartialContent)
		} else {
			writer.WriteHeader(http.StatusOK)
		}
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("response writer is not a flusher")
			return
		}
		flusher.Flush()
		limit := cfg.size
		if cfg.trickle > 0 {
			limit = cfg.trickle
		}
		chunk := make([]byte, 8*1024)
		for written := int64(0); written < limit; {
			n := int64(len(chunk))
			if limit-written < n {
				n = limit - written
			}
			if _, err := writer.Write(chunk[:n]); err != nil {
				return
			}
			flusher.Flush()
			written += n
		}
		if cfg.trickle > 0 {
			<-request.Context().Done()
		}
	}))
}

func probeTestSource(t *testing.T, kind Kind, key string, baseURL string, official bool) Source {
	t.Helper()
	source, err := NewSource(kind, key, baseURL, official)
	if err != nil {
		t.Fatalf("NewSource(%q) error = %v", key, err)
	}
	return source
}

// probeIndexSource 用测试服务器地址构造带 packages 前缀的包索引源；探针应拼在 packages 前缀之后。
func probeIndexSource(t *testing.T, key string, serverURL string, official bool) Source {
	t.Helper()
	source, err := NewPackageIndexSource(PackageIndexSpec{
		Key:          key,
		BaseURL:      serverURL + "/simple/",
		SimpleBase:   serverURL + "/simple",
		PackagesBase: serverURL + "/packages/",
		Official:     official,
	})
	if err != nil {
		t.Fatalf("NewPackageIndexSource(%q) error = %v", key, err)
	}
	return source
}

// probeTestPlan 用测试源替换默认目录中 kind 的全部源，其余 Kind 沿用默认目录。
func probeTestPlan(t *testing.T, kind Kind, sources []Source) Plan {
	t.Helper()
	all := make([]Source, 0, len(sources)+16)
	for _, source := range allDefaultSources(t) {
		if source.Kind() != kind {
			all = append(all, source)
		}
	}
	all = append(all, sources...)
	catalog, err := NewCatalog(all)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	policy, err := NewPolicy(PolicySpec{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := BuildPlan(catalog, policy, kind)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	return plan
}

func mustProber(t *testing.T, options ...ProbeOption) *Prober {
	t.Helper()
	prober, err := NewProber(options...)
	if err != nil {
		t.Fatalf("NewProber() error = %v", err)
	}
	return prober
}

// serialReportRecorder 记录回调顺序，并在回调并发时报错。
type serialReportRecorder struct {
	t      *testing.T
	active atomic.Int32
	mu     sync.Mutex // 保护 keys
	keys   []string
	onKey  func(key string)
}

func (r *serialReportRecorder) report(result ProbeResult) {
	if !r.active.CompareAndSwap(0, 1) {
		r.t.Error("report callback invoked concurrently")
	}
	r.mu.Lock()
	r.keys = append(r.keys, result.Source.Key())
	r.mu.Unlock()
	if r.onKey != nil {
		r.onKey(result.Source.Key())
	}
	r.active.Store(0)
}

func (r *serialReportRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func resultByKey(t *testing.T, results []ProbeResult, key string) ProbeResult {
	t.Helper()
	for _, result := range results {
		if result.Source.Key() == key {
			return result
		}
	}
	t.Fatalf("results %#v have no source %q", results, key)
	return ProbeResult{}
}

func TestProber_RanksByThroughput(t *testing.T) {
	const (
		slowRate   int64 = 50 * 1000
		mediumRate int64 = 500 * 1000
		fastRate   int64 = 5 * 1000 * 1000
	)
	clock := newFakeProbeClock()
	mediumGate := make(chan struct{})
	slowGate := make(chan struct{})
	fast := probeServer(t, probeServerConfig{size: probeTestWindow})
	medium := probeServer(t, probeServerConfig{size: probeTestWindow, gate: mediumGate})
	slow := probeServer(t, probeServerConfig{size: probeTestWindow, gate: slowGate})
	defer fast.Close()
	defer medium.Close()
	defer slow.Close()

	rates := map[string]int64{
		mustURL(t, fast.URL).Host:   fastRate,
		mustURL(t, medium.URL).Host: mediumRate,
		mustURL(t, slow.URL).Host:   slowRate,
	}
	trusted := trustedClientForServers(t, fast, medium, slow)
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		response, err := trusted.Do(request)
		if err != nil {
			return nil, err
		}
		rate, ok := rates[request.URL.Host]
		if !ok {
			t.Errorf("unexpected probe host %q", request.URL.Host)
			return response, nil
		}
		response.Body = &throttledBody{ReadCloser: response.Body, clock: clock, rate: rate}
		return response, nil
	})

	plan := probeTestPlan(t, KindUV, []Source{
		probeTestSource(t, KindUV, "slow", slow.URL, false),
		probeTestSource(t, KindUV, "medium", medium.URL, false),
		probeTestSource(t, KindUV, "fast", fast.URL, true),
	})
	// 三个源的测量区间串行化：慢源要等中速源报告完成后才发响应头，
	// 中速源要等快源报告完成，这样虚拟时钟的推进只归属于正在被读的那个源。
	recorder := &serialReportRecorder{t: t, onKey: func(key string) {
		switch key {
		case "fast":
			close(mediumGate)
		case "medium":
			close(slowGate)
		}
	}}
	prober := mustProber(t,
		WithProbeClient(client),
		WithProbeClock(clock.Now),
		WithProbeBudget(time.Minute),
	)
	results, err := prober.Probe(
		t.Context(),
		plan,
		ProbeTarget{Kind: KindUV, Path: "0.12.3/uv-x86_64-pc-windows-msvc.zip", WindowBytes: probeTestWindow},
		recorder.report,
	)
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	if got, want := recorder.recorded(), []string{"fast", "medium", "slow"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("report order = %v, want %v", got, want)
	}
	if got, want := len(results), 3; got != want {
		t.Fatalf("len(results) = %d, want %d", got, want)
	}
	for index, want := range []string{"slow", "medium", "fast"} {
		if got := results[index].Source.Key(); got != want {
			t.Fatalf("results[%d].Source.Key() = %q, want %q (plan order)", index, got, want)
		}
	}
	expected := map[string]int64{"slow": slowRate, "medium": mediumRate, "fast": fastRate}
	for key, rate := range expected {
		result := resultByKey(t, results, key)
		if !result.OK || result.Err != nil {
			t.Fatalf("%s result = %+v, want OK without error", key, result)
		}
		if result.Bytes != probeTestWindow {
			t.Fatalf("%s Bytes = %d, want %d", key, result.Bytes, probeTestWindow)
		}
		low, high := rate-rate/50, rate+rate/50
		if result.BytesPerSecond < low || result.BytesPerSecond > high {
			t.Fatalf("%s BytesPerSecond = %d, want within [%d, %d]", key, result.BytesPerSecond, low, high)
		}
	}
	if !(resultByKey(t, results, "slow").BytesPerSecond <
		resultByKey(t, results, "medium").BytesPerSecond &&
		resultByKey(t, results, "medium").BytesPerSecond <
			resultByKey(t, results, "fast").BytesPerSecond) {
		t.Fatalf("throughput order = %+v, want slow < medium < fast", results)
	}
}

func TestProber_BudgetCutsSlowSource(t *testing.T) {
	const trickleBytes int64 = 4 * 1024
	fast := probeServer(t, probeServerConfig{size: probeTestWindow})
	trickle := probeServer(t, probeServerConfig{size: probeTestWindow, trickle: trickleBytes})
	stalled := probeServer(t, probeServerConfig{stallHeaders: true})
	defer fast.Close()
	defer trickle.Close()
	defer stalled.Close()

	var openBodies atomic.Int32
	trusted := trustedClientForServers(t, fast, trickle, stalled)
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		response, err := trusted.Do(request)
		if err != nil {
			return nil, err
		}
		openBodies.Add(1)
		response.Body = &countingBody{ReadCloser: response.Body, open: &openBodies}
		return response, nil
	})
	plan := probeTestPlan(t, KindUV, []Source{
		probeTestSource(t, KindUV, "trickle", trickle.URL, false),
		probeTestSource(t, KindUV, "stalled", stalled.URL, false),
		probeTestSource(t, KindUV, "fast", fast.URL, true),
	})
	prober := mustProber(t,
		WithProbeClient(client),
		WithProbeClock(newFakeProbeClock().Now),
		WithProbeBudget(250*time.Millisecond),
	)
	results, err := prober.Probe(
		t.Context(),
		plan,
		ProbeTarget{Kind: KindUV, Path: "0.12.3/uv-x86_64-pc-windows-msvc.zip", WindowBytes: probeTestWindow},
		nil,
	)
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	if got := openBodies.Load(); got != 0 {
		t.Fatalf("open response bodies after Probe() = %d, want 0", got)
	}
	fastResult := resultByKey(t, results, "fast")
	if !fastResult.OK || fastResult.Err != nil || fastResult.Bytes != probeTestWindow {
		t.Fatalf("fast result = %+v, want OK with full window", fastResult)
	}
	trickleResult := resultByKey(t, results, "trickle")
	if !trickleResult.OK || trickleResult.Err != nil {
		t.Fatalf("trickle result = %+v, want OK counted from bytes read before budget", trickleResult)
	}
	if trickleResult.Bytes <= 0 || trickleResult.Bytes > trickleBytes {
		t.Fatalf("trickle Bytes = %d, want within (0, %d]", trickleResult.Bytes, trickleBytes)
	}
	if trickleResult.BytesPerSecond <= 0 {
		t.Fatalf("trickle BytesPerSecond = %d, want positive", trickleResult.BytesPerSecond)
	}
	stalledResult := resultByKey(t, results, "stalled")
	if stalledResult.OK || stalledResult.Err == nil {
		t.Fatalf("stalled result = %+v, want failure without headers", stalledResult)
	}
	if stalledResult.Bytes != 0 || stalledResult.BytesPerSecond != 0 || stalledResult.TTFB != 0 {
		t.Fatalf("stalled result = %+v, want zero measurements", stalledResult)
	}
}

func TestProber_FailureIsNotFatal(t *testing.T) {
	healthy := probeServer(t, probeServerConfig{size: probeTestWindow})
	failing := probeServer(t, probeServerConfig{status: http.StatusInternalServerError})
	defer healthy.Close()
	defer failing.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	refusedURL := "https://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	plan := probeTestPlan(t, KindPython, []Source{
		probeTestSource(t, KindPython, "failing", failing.URL, false),
		probeTestSource(t, KindPython, "refused", refusedURL, false),
		probeTestSource(t, KindPython, "healthy", healthy.URL, true),
	})
	recorder := &serialReportRecorder{t: t}
	prober := mustProber(t,
		WithProbeClient(trustedClientForServers(t, healthy, failing)),
		WithProbeClock(newFakeProbeClock().Now),
	)
	results, err := prober.Probe(
		t.Context(),
		plan,
		ProbeTarget{Kind: KindPython, Path: "20260807/cpython-3.12.13.tar.gz", WindowBytes: probeTestWindow},
		recorder.report,
	)
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil even though two probes failed", err)
	}
	if got, want := len(recorder.recorded()), 3; got != want {
		t.Fatalf("report calls = %d, want %d", got, want)
	}
	if got, want := len(results), 3; got != want {
		t.Fatalf("len(results) = %d, want %d", got, want)
	}
	healthyResult := resultByKey(t, results, "healthy")
	if !healthyResult.OK || healthyResult.Err != nil || healthyResult.Bytes != probeTestWindow {
		t.Fatalf("healthy result = %+v, want OK with full window", healthyResult)
	}
	for _, key := range []string{"failing", "refused"} {
		result := resultByKey(t, results, key)
		if result.OK || result.Err == nil {
			t.Fatalf("%s result = %+v, want failure recorded in Err", key, result)
		}
		if result.TTFB != 0 || result.BytesPerSecond != 0 || result.Bytes != 0 || result.AcceptRanges {
			t.Fatalf("%s result = %+v, want zero measurements on failure", key, result)
		}
		if strings.Contains(result.Err.Error(), "127.0.0.1") {
			t.Fatalf("%s Err = %q, leaks source address", key, result.Err)
		}
	}
	if failingResult := resultByKey(t, results, "failing"); !strings.Contains(failingResult.Err.Error(), "500") {
		t.Fatalf("failing Err = %q, want status code", failingResult.Err)
	}
}

func TestProber_ReportsAcceptRanges(t *testing.T) {
	partial := probeServer(t, probeServerConfig{size: probeTestWindow, status: http.StatusPartialContent})
	advertised := probeServer(t, probeServerConfig{size: probeTestWindow, acceptRanges: true})
	plain := probeServer(t, probeServerConfig{size: probeTestWindow})
	defer partial.Close()
	defer advertised.Close()
	defer plain.Close()

	var rangeHeaders sync.Map
	trusted := trustedClientForServers(t, partial, advertised, plain)
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		rangeHeaders.Store(request.URL.Host, request.Header.Get("Range"))
		return trusted.Do(request)
	})
	plan := probeTestPlan(t, KindPackageIndex, []Source{
		probeIndexSource(t, "partial", partial.URL, false),
		probeIndexSource(t, "advertised", advertised.URL, false),
		probeIndexSource(t, "plain", plain.URL, true),
	})
	prober := mustProber(t,
		WithProbeClient(client),
		WithProbeClock(newFakeProbeClock().Now),
	)
	results, err := prober.Probe(
		t.Context(),
		plan,
		ProbeTarget{Kind: KindPackageIndex, Path: "ab/cd/numpy.whl", WindowBytes: probeTestWindow},
		nil,
	)
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	tests := []struct {
		key  string
		want bool
	}{
		{key: "partial", want: true},
		{key: "advertised", want: true},
		{key: "plain", want: false},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			result := resultByKey(t, results, test.key)
			if !result.OK || result.Err != nil {
				t.Fatalf("result = %+v, want OK", result)
			}
			if result.AcceptRanges != test.want {
				t.Fatalf("AcceptRanges = %t, want %t", result.AcceptRanges, test.want)
			}
		})
	}
	wantRange := "bytes=0-" + strconv.FormatInt(probeTestWindow-1, 10)
	for _, server := range []*httptest.Server{partial, advertised, plain} {
		got, ok := rangeHeaders.Load(mustURL(t, server.URL).Host)
		if !ok || got != wantRange {
			t.Fatalf("Range header for %s = %v, want %q", server.URL, got, wantRange)
		}
	}
}

func TestProber_ZeroWindowMeasuresOnlyTTFB(t *testing.T) {
	clock := newFakeProbeClock()
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if got := request.Header.Get("Range"); got != "" {
			t.Errorf("Range = %q, want empty for zero window", got)
		}
		clock.Advance(40 * time.Millisecond)
		writer.Header().Set("Content-Length", "1048576")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	plan := probeTestPlan(t, KindGit, []Source{
		probeTestSource(t, KindGit, "only", server.URL+"/AUTO-MAS.git", true),
	})
	prober := mustProber(t,
		WithProbeClient(trustedClientForServers(t, server)),
		WithProbeClock(clock.Now),
	)
	results, err := prober.Probe(
		t.Context(),
		plan,
		ProbeTarget{Kind: KindGit, Path: "info/refs?service=git-upload-pack"},
		nil,
	)
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	result := resultByKey(t, results, "only")
	if !result.OK || result.Err != nil {
		t.Fatalf("result = %+v, want OK", result)
	}
	if result.TTFB != 40*time.Millisecond {
		t.Fatalf("TTFB = %s, want 40ms", result.TTFB)
	}
	if result.Bytes != 0 || result.BytesPerSecond != 0 {
		t.Fatalf("result = %+v, want no throughput for zero window", result)
	}
}

func TestProber_CancelledContextIsAnError(t *testing.T) {
	stalled := probeServer(t, probeServerConfig{stallHeaders: true})
	defer stalled.Close()
	plan := probeTestPlan(t, KindUV, []Source{
		probeTestSource(t, KindUV, "stalled", stalled.URL, true),
	})
	prober := mustProber(t,
		WithProbeClient(trustedClientForServers(t, stalled)),
		WithProbeClock(newFakeProbeClock().Now),
		WithProbeBudget(time.Minute),
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	results, err := prober.Probe(
		ctx,
		plan,
		ProbeTarget{Kind: KindUV, Path: "0.12.3/uv.zip", WindowBytes: probeTestWindow},
		nil,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe() error = %v, want context.Canceled in chain", err)
	}
	if results != nil {
		t.Fatalf("results = %+v, want nil on cancellation", results)
	}
}

func TestProber_RejectsInvalidRequests(t *testing.T) {
	prober := mustProber(t,
		WithProbeClient(httpClientFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network must not be touched")
		})),
		WithProbeClock(newFakeProbeClock().Now),
	)
	offlinePolicy, err := NewPolicy(PolicySpec{Offline: true})
	if err != nil {
		t.Fatalf("NewPolicy(offline) error = %v", err)
	}
	offlinePlan, err := BuildPlan(mustDefaultCatalog(t), offlinePolicy, KindUV)
	if err != nil {
		t.Fatalf("BuildPlan(offline) error = %v", err)
	}
	online := mustOnlinePlan(t, KindUV)
	tests := []struct {
		name   string
		plan   Plan
		target ProbeTarget
	}{
		{name: "offline plan", plan: offlinePlan, target: ProbeTarget{Kind: KindUV, Path: "x"}},
		{name: "zero plan", plan: Plan{}, target: ProbeTarget{Kind: KindUV, Path: "x"}},
		{name: "kind mismatch", plan: online, target: ProbeTarget{Kind: KindGit, Path: "x"}},
		{name: "empty path", plan: online, target: ProbeTarget{Kind: KindUV}},
		{name: "path with newline", plan: online, target: ProbeTarget{Kind: KindUV, Path: "a\nb"}},
		{name: "negative window", plan: online, target: ProbeTarget{Kind: KindUV, Path: "x", WindowBytes: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, err := prober.Probe(t.Context(), test.plan, test.target, nil)
			if !errors.Is(err, ErrInvalidProbeRequest) {
				t.Fatalf("Probe() error = %v, want ErrInvalidProbeRequest", err)
			}
			if results != nil {
				t.Fatalf("results = %+v, want nil", results)
			}
		})
	}
}

func TestNewProber_ValidatesOptions(t *testing.T) {
	client := httpClientFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	tests := []struct {
		name    string
		options []ProbeOption
	}{
		{name: "nil option", options: []ProbeOption{nil}},
		{name: "zero budget", options: []ProbeOption{WithProbeBudget(0)}},
		{name: "negative budget", options: []ProbeOption{WithProbeBudget(-time.Second)}},
		{name: "duplicate budget", options: []ProbeOption{WithProbeBudget(time.Second), WithProbeBudget(time.Second)}},
		{name: "nil client", options: []ProbeOption{WithProbeClient(nil)}},
		{name: "duplicate client", options: []ProbeOption{WithProbeClient(client), WithProbeClient(client)}},
		{name: "nil clock", options: []ProbeOption{WithProbeClock(nil)}},
		{name: "duplicate clock", options: []ProbeOption{WithProbeClock(time.Now), WithProbeClock(time.Now)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prober, err := NewProber(test.options...)
			if !errors.Is(err, ErrInvalidProbeOption) {
				t.Fatalf("NewProber() error = %v, want ErrInvalidProbeOption", err)
			}
			if prober != nil {
				t.Fatalf("NewProber() = %+v, want nil", prober)
			}
		})
	}
	prober, err := NewProber()
	if err != nil {
		t.Fatalf("NewProber() default error = %v", err)
	}
	if prober.budget != defaultProbeBudget {
		t.Fatalf("default budget = %s, want %s", prober.budget, defaultProbeBudget)
	}
	if prober.client == nil || prober.clock == nil {
		t.Fatalf("default prober = %+v, want client and clock", prober)
	}
}

func TestProbeThroughput_ClampsElapsed(t *testing.T) {
	tests := []struct {
		name    string
		bytes   int64
		elapsed time.Duration
		want    int64
	}{
		{name: "no bytes", bytes: 0, elapsed: time.Second, want: 0},
		{name: "one second", bytes: 1000, elapsed: time.Second, want: 1000},
		{name: "half second", bytes: 1000, elapsed: 500 * time.Millisecond, want: 2000},
		{name: "zero elapsed clamps", bytes: 1000, elapsed: 0, want: 1000 * int64(time.Second/minProbeElapsed)},
		{name: "negative elapsed clamps", bytes: 1000, elapsed: -time.Second, want: 1000 * int64(time.Second/minProbeElapsed)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeThroughput(test.bytes, test.elapsed); got != test.want {
				t.Fatalf("probeThroughput(%d, %s) = %d, want %d", test.bytes, test.elapsed, got, test.want)
			}
		})
	}
}
