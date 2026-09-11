package relay

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWait = 20 * time.Second

// mirrorBehavior 描述假镜像对某个路径的故障注入；零值表示正常服务。
type mirrorBehavior struct {
	// status 非零时直接返回该状态码。
	status int
	// cutAt 大于零时写出 cutAt 字节后直接断开连接。
	cutAt int
	// hangAt 大于等于零且 hang 为真时写出 hangAt 字节后挂起，直到请求 ctx 结束。
	hang   bool
	hangAt int
	// corrupt 为真时翻转第一个字节。
	corrupt bool
	// failRangesFrom 大于零时，起点不小于它的 Range 请求返回 500。
	failRangesFrom int64
	// gate 非 nil 时请求在写响应前阻塞到 gate 被关闭（或请求 ctx 结束）。
	gate chan struct{}
	// entered 非 nil 时每次请求进入即发一次信号（非阻塞，需足够缓冲）。
	entered chan struct{}
	// contentType 非空时作为响应 Content-Type。
	contentType string
	// jsonBody 非 nil 时，Accept 含 json 的请求改回它。
	jsonBody []byte
}

type recordedRequest struct {
	method    string
	path      string
	rangeSpec string
	accept    string
}

// fakeMirror 是支持 Range、可注入故障的 TLS 假镜像。
type fakeMirror struct {
	server *httptest.Server
	ranges bool

	mu        sync.Mutex // 保护 files、behaviors、requests。
	files     map[string][]byte
	behaviors map[string]*mirrorBehavior
	requests  []recordedRequest
}

func newFakeMirror(t *testing.T, ranges bool) *fakeMirror {
	t.Helper()
	mirror := &fakeMirror{
		ranges:    ranges,
		files:     make(map[string][]byte),
		behaviors: make(map[string]*mirrorBehavior),
	}
	mirror.server = httptest.NewTLSServer(http.HandlerFunc(mirror.handle))
	t.Cleanup(mirror.server.Close)
	return mirror
}

func (m *fakeMirror) url() string { return m.server.URL }

func (m *fakeMirror) put(path string, data []byte) {
	m.mu.Lock()
	m.files[path] = data
	m.mu.Unlock()
}

func (m *fakeMirror) behave(path string, behavior mirrorBehavior) {
	m.mu.Lock()
	copied := behavior
	m.behaviors[path] = &copied
	m.mu.Unlock()
}

func (m *fakeMirror) recorded() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedRequest(nil), m.requests...)
}

func (m *fakeMirror) count(path string) int {
	total := 0
	for _, request := range m.recorded() {
		if request.path == path {
			total++
		}
	}
	return total
}

func (m *fakeMirror) countAll() int { return len(m.recorded()) }

func (m *fakeMirror) handle(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	m.mu.Lock()
	m.requests = append(m.requests, recordedRequest{
		method:    request.Method,
		path:      path,
		rangeSpec: request.Header.Get("Range"),
		accept:    request.Header.Get("Accept"),
	})
	behavior := m.behaviors[path]
	data, exists := m.files[path]
	m.mu.Unlock()
	if behavior == nil {
		behavior = &mirrorBehavior{}
	}
	if behavior.entered != nil {
		select {
		case behavior.entered <- struct{}{}:
		default:
		}
	}
	if behavior.gate != nil {
		select {
		case <-behavior.gate:
		case <-request.Context().Done():
			return
		}
	}
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if behavior.status != 0 {
		writer.WriteHeader(behavior.status)
		return
	}
	contentType := behavior.contentType
	if behavior.jsonBody != nil && strings.Contains(request.Header.Get("Accept"), "json") {
		data = behavior.jsonBody
		contentType = "application/vnd.pypi.simple.v1+json"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	writer.Header().Set("Content-Type", contentType)
	if m.ranges {
		writer.Header().Set("Accept-Ranges", "bytes")
	}
	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		writer.WriteHeader(http.StatusOK)
		return
	}
	start, end := int64(0), int64(len(data)-1)
	status := http.StatusOK
	if spec := request.Header.Get("Range"); spec != "" && m.ranges {
		parsedStart, parsedEnd, ok := parseRangeSpec(spec, int64(len(data)))
		if !ok {
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end = parsedStart, parsedEnd
		status = http.StatusPartialContent
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	if behavior.failRangesFrom > 0 && request.Header.Get("Range") != "" && start >= behavior.failRangesFrom {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	body := append([]byte(nil), data[start:end+1]...)
	if behavior.corrupt && len(body) > 0 {
		body[0] ^= 0xff
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	controller := http.NewResponseController(writer)
	switch {
	case behavior.cutAt > 0 && behavior.cutAt < len(body):
		_, _ = writer.Write(body[:behavior.cutAt])
		_ = controller.Flush()
		panic(http.ErrAbortHandler)
	case behavior.hang:
		if behavior.hangAt > len(body) {
			behavior.hangAt = len(body)
		}
		_, _ = writer.Write(body[:behavior.hangAt])
		_ = controller.Flush()
		<-request.Context().Done()
	default:
		_, _ = writer.Write(body)
	}
}

func parseRangeSpec(spec string, size int64) (int64, int64, bool) {
	spec = strings.TrimPrefix(spec, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

// trustedClient 构造只信任 httptest 证书、强制 HTTP/1.1 的上游客户端。
func trustedClient(t *testing.T, mirrors ...*fakeMirror) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	for _, mirror := range mirrors {
		roots.AddCert(mirror.server.Certificate())
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return newHTTPClientWithTransport(transport)
}

func packagesUpstream(mirror *fakeMirror, key string) Upstream {
	return Upstream{
		Key:         key,
		Base:        mirror.url() + "/packages/",
		AcceptRange: mirror.ranges,
		RewriteFrom: mirror.url() + "/packages/",
	}
}

func simpleUpstream(mirror *fakeMirror, key string) Upstream {
	return Upstream{
		Key:         key,
		Base:        mirror.url() + "/simple/",
		RewriteFrom: mirror.url() + "/packages/",
	}
}

func pythonUpstream(mirror *fakeMirror, key string) Upstream {
	return Upstream{Key: key, Base: mirror.url() + "/python/", AcceptRange: mirror.ranges}
}

// testFixture 把中继与它的记录型依赖捆在一起。
type testFixture struct {
	server   *Server
	client   *http.Client
	progress *progressRecorder
	logs     *logRecorder
	clock    *fakeClock
}

type fixtureOptions struct {
	cfg       Config
	deps      Deps
	internals internals
}

func startFixture(t *testing.T, options fixtureOptions, mirrors ...*fakeMirror) *testFixture {
	t.Helper()
	cfg := options.cfg
	if cfg.StagingDir == "" {
		cfg.StagingDir = filepath.Join(t.TempDir(), "relay")
	}
	deps := options.deps
	fixture := &testFixture{
		client: &http.Client{},
	}
	if deps.Client == nil {
		deps.Client = trustedClient(t, mirrors...)
	}
	if deps.Clock == nil {
		fixture.clock = newFakeClock()
		deps.Clock = fixture.clock.now
	}
	if deps.Progress == nil {
		fixture.progress = &progressRecorder{}
		deps.Progress = fixture.progress.record
	}
	if deps.Logger == nil {
		fixture.logs = &logRecorder{}
		deps.Logger = fixture.logs.record
	}
	server, err := start(t.Context(), cfg, deps, options.internals)
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	fixture.server = server
	return fixture
}

func (f *testFixture) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	return f.do(t, http.MethodGet, path, nil)
}

func (f *testFixture) do(t *testing.T, method, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, f.server.BaseURL()+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, path, err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", method, path, err)
	}
	body, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read body of %s %s: %v / %v", method, path, err, closeErr)
	}
	return response, body
}

// fetchResult 是并发请求的结果，供 barrier 类测试收集。
type fetchResult struct {
	status int
	body   []byte
	err    error
}

func (f *testFixture) getAsync(path string) <-chan fetchResult {
	results := make(chan fetchResult, 1)
	go func() {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.server.BaseURL()+path, nil)
		if err != nil {
			results <- fetchResult{err: err}
			return
		}
		response, err := f.client.Do(request)
		if err != nil {
			results <- fetchResult{err: err}
			return
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		results <- fetchResult{status: response.StatusCode, body: body, err: err}
	}()
	return results
}

func awaitResult(t *testing.T, results <-chan fetchResult, what string) fetchResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		return fetchResult{}
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

type progressRecorder struct {
	mu      sync.Mutex // 保护 events 与 failAfter。
	events  []Progress
	failErr error
}

func (r *progressRecorder) record(progress Progress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, progress)
	return r.failErr
}

func (r *progressRecorder) snapshot() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Progress(nil), r.events...)
}

type logEntry struct {
	level   string
	message string
	fields  map[string]any
}

type logRecorder struct {
	mu      sync.Mutex // 保护 entries。
	entries []logEntry
}

func (r *logRecorder) record(level, message string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for key, value := range fields {
		copied[key] = value
	}
	r.entries = append(r.entries, logEntry{level: level, message: message, fields: copied})
}

func (r *logRecorder) outcomes(source string) []Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	var outcomes []Outcome
	for _, entry := range r.entries {
		if entry.fields["source"] != source {
			continue
		}
		if outcome, ok := entry.fields["outcome"].(Outcome); ok {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes
}

// fakeClock 是可手动推进的时钟；step 非零时每次读取自动前进。
type fakeClock struct {
	mu   sync.Mutex
	now_ time.Time
	step time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now_: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.now_
	c.now_ = c.now_.Add(c.step)
	return current
}

func (c *fakeClock) advance(delta time.Duration) {
	c.mu.Lock()
	c.now_ = c.now_.Add(delta)
	c.mu.Unlock()
}

func (c *fakeClock) setStep(step time.Duration) {
	c.mu.Lock()
	c.step = step
	c.mu.Unlock()
}

// randomBytes 生成确定性的测试内容，避免全零内容掩盖拼接错位。
func testContent(size int, seed byte) []byte {
	data := make([]byte, size)
	value := uint32(seed) + 1
	for i := range data {
		value = value*1664525 + 1013904223
		data[i] = byte(value >> 24)
	}
	return data
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func itemFor(path string, data []byte) Item {
	return Item{Path: path, Size: int64(len(data)), SHA256: sha256Hex(data)}
}

// blockingFetcher 是同包测试用的取回替身：进入即发信号，随后阻塞到 ctx 结束。
type blockingFetcher struct {
	entered chan struct{}
}

func (f *blockingFetcher) fetch(ctx context.Context, _ Route, _ string) (string, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", errors.Join(errors.New("fetch aborted"), ctx.Err())
}
