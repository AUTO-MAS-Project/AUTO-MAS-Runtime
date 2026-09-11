package relay

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loopbackConfig(mirror *fakeMirror) Config {
	return Config{
		Upstreams: map[Route][]Upstream{
			RoutePackages: {packagesUpstream(mirror, "a")},
			RouteSimple:   {simpleUpstream(mirror, "a")},
			RoutePython:   {pythonUpstream(mirror, "a")},
		},
	}
}

func TestServer_ListensOnLoopbackOnly(t *testing.T) {
	mirror := newFakeMirror(t, true)
	fixture := startFixture(t, fixtureOptions{cfg: loopbackConfig(mirror)}, mirror)

	base := fixture.server.BaseURL()
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("BaseURL() = %q, want http://127.0.0.1:<port>", base)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse BaseURL: %v", err)
	}
	address, ok := fixture.server.listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("listener addr = %v, want 127.0.0.1", fixture.server.listener.Addr())
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	if conn, err := dialer.DialContext(t.Context(), "tcp6", net.JoinHostPort("::1", parsed.Port())); err == nil {
		_ = conn.Close()
		t.Fatalf("dial [::1]:%s succeeded, want refusal", parsed.Port())
	}
	conn, err := dialer.DialContext(t.Context(), "tcp4", parsed.Host)
	if err != nil {
		t.Fatalf("dial %s: %v", parsed.Host, err)
	}
	_ = conn.Close()
}

func TestServer_RejectsTraversal(t *testing.T) {
	mirror := newFakeMirror(t, true)
	mirror.put("packages/a/b/x.whl", []byte("x"))
	fixture := startFixture(t, fixtureOptions{cfg: loopbackConfig(mirror)}, mirror)

	tests := []struct {
		name string
		path string
	}{
		{name: "dot dot segment", path: "/packages/../secret"},
		{name: "nested dot dot", path: "/packages/a/../../secret"},
		{name: "encoded dot dot", path: "/packages/%2e%2e/secret"},
		{name: "mixed encoded dot dot", path: "/packages/.%2e/secret"},
		{name: "empty segment", path: "/packages//x.whl"},
		{name: "interior empty segment", path: "/packages/a//x.whl"},
		{name: "single dot segment", path: "/packages/a/./x.whl"},
		{name: "encoded slash escapes segment", path: "/python/..%2fsecret"},
		{name: "backslash segment", path: "/python/a%5c..%5csecret"},
		{name: "trailing slash on file route", path: "/packages/a/b/"},
		{name: "simple dot dot", path: "/simple/../x/"},
		{name: "simple nested name", path: "/simple/a/b/"},
		{name: "prefix only", path: "/packages/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, _ := fixture.do(t, http.MethodGet, test.path, nil)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404", test.path, response.StatusCode)
			}
		})
	}
	if got := mirror.countAll(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func TestServer_UnknownRouteIs404(t *testing.T) {
	mirror := newFakeMirror(t, true)
	fixture := startFixture(t, fixtureOptions{cfg: loopbackConfig(mirror)}, mirror)

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "root", method: http.MethodGet, path: "/", want: http.StatusNotFound},
		{name: "unknown prefix", method: http.MethodGet, path: "/files/x.whl", want: http.StatusNotFound},
		{name: "prefix without slash", method: http.MethodGet, path: "/packagesx/x.whl", want: http.StatusNotFound},
		{name: "prefix as file", method: http.MethodGet, path: "/packages", want: http.StatusNotFound},
		{name: "post on packages", method: http.MethodPost, path: "/packages/a/x.whl", want: http.StatusMethodNotAllowed},
		{name: "head on simple", method: http.MethodHead, path: "/simple/six/", want: http.StatusMethodNotAllowed},
		{name: "put on python", method: http.MethodPut, path: "/python/20260807/x.tar.gz", want: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, _ := fixture.do(t, test.method, test.path, nil)
			if response.StatusCode != test.want {
				t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.StatusCode, test.want)
			}
		})
	}
	if got := mirror.countAll(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func TestServer_CloseWaitsForHandlers(t *testing.T) {
	mirror := newFakeMirror(t, true)
	fetcher := &blockingFetcher{entered: make(chan struct{}, 1)}
	stagingDir := filepath.Join(t.TempDir(), "relay")
	fixture := startFixture(t, fixtureOptions{
		cfg:       Config{StagingDir: stagingDir, Upstreams: loopbackConfig(mirror).Upstreams},
		internals: internals{fetcher: fetcher},
	}, mirror)
	if err := os.WriteFile(filepath.Join(stagingDir, "foreign.bin"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.server.staging.track(filepath.Join(stagingDir, "staged.part"))
	if err := os.WriteFile(filepath.Join(stagingDir, "staged.part"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	results := fixture.getAsync("/packages/a/b/x.whl")
	awaitSignal(t, fetcher.entered, "fetcher entry")

	if err := fixture.server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := fixture.server.inflight.Load(); got != 0 {
		t.Fatalf("inflight handlers after Close = %d, want 0", got)
	}
	result := awaitResult(t, results, "in-flight request")
	if result.err == nil && result.status != http.StatusBadGateway {
		t.Fatalf("in-flight request status = %d, want 502 or connection error", result.status)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "staged.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file after Close: err = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "foreign.bin")); err != nil {
		t.Fatalf("foreign file after Close: err = %v, want kept", err)
	}
	if err := fixture.server.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := fixture.client.Get(fixture.server.BaseURL() + "/packages/a/b/x.whl"); err == nil {
		t.Fatal("request after Close succeeded, want connection refused")
	}
}

func TestServer_ContextCancelCloses(t *testing.T) {
	mirror := newFakeMirror(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	server, err := start(ctx, Config{
		StagingDir: filepath.Join(t.TempDir(), "relay"),
		Upstreams:  loopbackConfig(mirror).Upstreams,
	}, Deps{Client: trustedClient(t, mirror)}, internals{})
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	cancel()
	awaitSignal(t, server.closed, "server closed by ctx")
	if err := server.Close(); err != nil {
		t.Fatalf("Close() after ctx cancel error = %v", err)
	}
}

func TestStart_RejectsInvalidConfig(t *testing.T) {
	mirror := newFakeMirror(t, true)
	valid := loopbackConfig(mirror)
	stagingDir := filepath.Join(t.TempDir(), "relay")
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "relative staging dir", cfg: Config{StagingDir: "relay", Upstreams: valid.Upstreams}},
		{name: "no upstreams", cfg: Config{StagingDir: stagingDir}},
		{name: "http upstream", cfg: Config{StagingDir: stagingDir, Upstreams: map[Route][]Upstream{
			RoutePackages: {{Key: "a", Base: "http://127.0.0.1:1/packages/"}},
		}}},
		{name: "empty key", cfg: Config{StagingDir: stagingDir, Upstreams: map[Route][]Upstream{
			RoutePackages: {{Base: "https://example.invalid/packages/"}},
		}}},
		{name: "bad route", cfg: Config{StagingDir: stagingDir, Upstreams: map[Route][]Upstream{
			Route("wheels"): {{Key: "a", Base: "https://example.invalid/packages/"}},
		}}},
		{name: "item without sha", cfg: Config{StagingDir: stagingDir, Upstreams: valid.Upstreams,
			Items: []Item{{Path: "a/b/x.whl", Size: 1}}}},
		{name: "item traversal", cfg: Config{StagingDir: stagingDir, Upstreams: valid.Upstreams,
			Items: []Item{{Path: "../x.whl", Size: 1, SHA256: sha256Hex(nil)}}}},
		{name: "negative chunk size", cfg: Config{StagingDir: stagingDir, Upstreams: valid.Upstreams, ChunkSize: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := Start(t.Context(), test.cfg, Deps{})
			if server != nil {
				_ = server.Close()
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Start() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestServer_SetUpstreamsReplacesOrder 锁定运行期替换上游顺序：替换后的新请求从新首位取，
// 非法路由与非法上游被拒绝且不改变现有顺序。
func TestServer_SetUpstreamsReplacesOrder(t *testing.T) {
	content := testContent(50_000, 3)
	first := newFakeMirror(t, false)
	first.put(wheelPath, content)
	second := newFakeMirror(t, false)
	second.put(wheelPath, content)
	fixture := startFixture(t, fixtureOptions{
		cfg: packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, first, second),
	}, first, second)

	if err := fixture.server.SetUpstreams(RoutePackages, []Upstream{packagesUpstream(second, "b"), packagesUpstream(first, "a")}); err != nil {
		t.Fatalf("SetUpstreams() error = %v", err)
	}
	response, body := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("status = %d, body = %d bytes", response.StatusCode, len(body))
	}
	if got := second.count(wheelPath); got != 1 {
		t.Errorf("second mirror requests = %d, want 1 after reorder", got)
	}
	if got := first.count(wheelPath); got != 0 {
		t.Errorf("first mirror requests = %d, want 0 after reorder", got)
	}
	if err := fixture.server.SetUpstreams(Route("bogus"), nil); err == nil {
		t.Error("SetUpstreams(bogus route) error = nil, want error")
	}
	if err := fixture.server.SetUpstreams(RoutePackages, []Upstream{{Key: "x", Base: "http://insecure.example/"}}); err == nil {
		t.Error("SetUpstreams(http upstream) error = nil, want error")
	}
}
