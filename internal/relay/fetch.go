package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

var errAllSourcesFailed = errors.New("relay: all sources failed")

// fileSpec 描述一次取回的目标：登记过的 Item 带大小与摘要，未登记文件由 HEAD 补大小。
type fileSpec struct {
	key    fileKey
	name   string
	size   int64
	digest []byte
}

// engine 是逐文件取回引擎：维护登记制品、各路由的上游列表与失败剔除、文件槽位，
// 并把字节流交给 tracker 与 ledger。
type engine struct {
	cfg     Config
	client  httpClient
	timers  timerFactory
	logger  func(level, message string, fields map[string]any)
	staging *staging
	tracker *tracker
	ledger  *ledger
	baseURL string

	slots       chan struct{}
	slotWaiting chan struct{}

	mu        sync.Mutex // 保护 items、upstreams、strikes。
	items     map[string]Item
	upstreams map[Route][]Upstream
	strikes   map[Route]map[string]int
}

func newEngine(
	cfg Config,
	deps Deps,
	private internals,
	staging *staging,
	baseURL string,
	onStop func(),
) *engine {
	e := &engine{
		cfg:         cfg,
		client:      deps.Client,
		timers:      private.timers,
		logger:      deps.Logger,
		staging:     staging,
		ledger:      newLedger(),
		baseURL:     baseURL,
		slots:       make(chan struct{}, cfg.MaxFiles),
		slotWaiting: private.slotWaiting,
		items:       make(map[string]Item, len(cfg.Items)),
		upstreams:   make(map[Route][]Upstream, len(cfg.Upstreams)),
		strikes:     make(map[Route]map[string]int, len(cfg.Upstreams)),
	}
	e.tracker = newTracker(deps.Clock, deps.Progress, onStop)
	for route, list := range cfg.Upstreams {
		e.upstreams[route] = append([]Upstream(nil), list...)
		e.strikes[route] = make(map[string]int)
	}
	e.register(cfg.Items)
	return e
}

// register 追加登记制品；已登记路径不重复计入总量，非法条目记日志后跳过。
func (e *engine) register(items []Item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, item := range items {
		if err := validateItem(item); err != nil {
			e.log("warning", "relay item rejected", map[string]any{"item": item.Path})
			continue
		}
		if _, exists := e.items[item.Path]; exists {
			continue
		}
		e.items[item.Path] = item
		e.tracker.addTotal(item.Size)
	}
}

func (e *engine) summary() Summary {
	return e.ledger.summary()
}

func (e *engine) log(level, message string, fields map[string]any) {
	if e.logger != nil {
		e.logger(level, message, fields)
	}
}

// fetch 取回一个文件到暂存目录：先占文件槽位，再按登记情况决定校验策略与取回方式。
func (e *engine) fetch(ctx context.Context, route Route, path string) (string, error) {
	if e.tracker.isStopped() {
		return "", errAllSourcesFailed
	}
	if err := e.acquireSlot(ctx); err != nil {
		return "", err
	}
	defer e.releaseSlot()
	spec := e.describe(route, path)
	if spec.digest == nil {
		e.probeSize(ctx, &spec)
	}
	return e.download(ctx, spec)
}

func (e *engine) acquireSlot(ctx context.Context) error {
	select {
	case e.slots <- struct{}{}:
		return nil
	default:
	}
	if e.slotWaiting != nil {
		select {
		case e.slotWaiting <- struct{}{}:
		default:
		}
	}
	select {
	case e.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *engine) releaseSlot() {
	<-e.slots
}

// describe 把请求路径映射到登记制品；登记路径与请求路径都是 URL 形态，按原样与解码后各比一次。
func (e *engine) describe(route Route, path string) fileSpec {
	spec := fileSpec{
		key:  fileKey{route: route, path: path},
		name: displayName(path),
	}
	if route != RoutePackages {
		return spec
	}
	e.mu.Lock()
	item, ok := e.items[path]
	if !ok {
		if decoded, err := url.PathUnescape(path); err == nil {
			item, ok = e.items[decoded]
		}
	}
	e.mu.Unlock()
	if ok {
		digest, err := hex.DecodeString(item.SHA256)
		if err == nil {
			spec.size = item.Size
			spec.digest = digest
		}
	}
	return spec
}

func displayName(path string) string {
	last := path
	if index := strings.LastIndex(path, "/"); index >= 0 {
		last = path[index+1:]
	}
	if decoded, err := url.PathUnescape(last); err == nil {
		return decoded
	}
	return last
}

// probeSize 对未登记文件 HEAD 首个可用上游，拿到 Content-Length 并入总量，并顺手更新该源的 Range 能力。
func (e *engine) probeSize(ctx context.Context, spec *fileSpec) {
	for _, upstream := range e.snapshotUpstreams(spec.key.route) {
		handle, outcome := e.open(ctx, http.MethodHead, upstream.Base+spec.key.path, nil)
		if outcome != "" {
			if outcome == OutcomeCancelled {
				return
			}
			continue
		}
		status := handle.response.StatusCode
		length := handle.response.ContentLength
		acceptRanges := strings.EqualFold(handle.response.Header.Get("Accept-Ranges"), "bytes")
		handle.close()
		if status != http.StatusOK {
			continue
		}
		if acceptRanges && !upstream.AcceptRange {
			e.markAcceptRange(spec.key.route, upstream.Key)
		}
		if length > 0 {
			spec.size = length
			e.tracker.addTotal(length)
		}
		return
	}
}

func (e *engine) snapshotUpstreams(route Route) []Upstream {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Upstream(nil), e.upstreams[route]...)
}

func (e *engine) markAcceptRange(route Route, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	list := e.upstreams[route]
	for i := range list {
		if list[i].Key == key {
			list[i].AcceptRange = true
		}
	}
}

// nextUpstream 返回该路由中第一个本文件尚未失败的源。
func (e *engine) nextUpstream(route Route, failed map[string]bool) (Upstream, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, upstream := range e.upstreams[route] {
		if !failed[upstream.Key] {
			return upstream, true
		}
	}
	return Upstream{}, false
}

// strike 记一次哈希不符；同一源在本中继生命周期内第二次即从该路由剔除。
func (e *engine) strike(route Route, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.strikes[route][key]++
	if e.strikes[route][key] < 2 {
		return
	}
	list := e.upstreams[route]
	kept := list[:0]
	for _, upstream := range list {
		if upstream.Key != key {
			kept = append(kept, upstream)
		}
	}
	e.upstreams[route] = kept
	e.log("warning", "relay upstream evicted after repeated checksum mismatch", map[string]any{
		"route":  route.String(),
		"source": key,
	})
}

// download 先尝试分片，再逐源单流取回直到成功或源耗尽；成功文件从 .part 改名为最终暂存路径。
func (e *engine) download(ctx context.Context, spec fileSpec) (string, error) {
	target := e.staging.pathFor(spec.key.route, spec.key.path)
	part := target + ".part"
	e.staging.track(target)
	e.staging.track(part)
	plan := &chunkPlan{failed: make(map[string]bool), contributions: make(map[string]int64)}
	if e.tryChunked(ctx, spec, part, plan) {
		if path, ok := e.publish(spec, part, target, plan.contributions); ok {
			return path, nil
		}
	}
	for ctx.Err() == nil {
		upstream, ok := e.nextUpstream(spec.key.route, plan.failed)
		if !ok {
			break
		}
		contributions := map[string]int64{}
		received, outcome := e.singleStream(ctx, spec, upstream, part, contributions)
		if outcome == "" {
			if spec.size == 0 {
				spec.size = received
			}
			if path, ok := e.publish(spec, part, target, contributions); ok {
				return path, nil
			}
			outcome = OutcomeNetwork
		}
		plan.markFailed(upstream.Key, outcome)
		e.tracker.reset(spec.key)
		e.log("warning", "relay attempt failed", map[string]any{
			"route": spec.key.route.String(), "item": spec.name,
			"source": upstream.Key, "outcome": outcome,
		})
		if outcome == OutcomeChecksumMismatch {
			e.strike(spec.key.route, upstream.Key)
		}
		if outcome == OutcomeCancelled {
			break
		}
	}
	e.tracker.fail(spec.key, spec.name)
	e.ledger.failed(spec.name, plan.attempts)
	return "", errAllSourcesFailed
}

// publish 把完整的 part 改名为最终暂存路径并记账；改名失败时记日志并让调用方换源重来。
func (e *engine) publish(
	spec fileSpec,
	part string,
	target string,
	contributions map[string]int64,
) (string, bool) {
	if err := os.Rename(part, target); err != nil {
		e.log("warning", "relay staged file publish failed", map[string]any{
			"item": spec.name, "error": err.Error(),
		})
		return "", false
	}
	e.ledger.delivered(spec.size, contributions)
	e.tracker.complete(spec.key, spec.name, topSource(contributions), spec.size)
	return target, true
}

// topSource 返回贡献字节最多的源；并列时取 key 字典序最小的，保证结果稳定。
func topSource(contributions map[string]int64) string {
	best := ""
	var bestCount int64 = -1
	for key, count := range contributions {
		if count > bestCount || (count == bestCount && key < best) {
			best, bestCount = key, count
		}
	}
	return best
}

// singleStream 从一个源整文件 GET 到 part；contributions 记录该源实际贡献的字节。
// 返回实际收到的字节数与结局，结局为空即成功。
func (e *engine) singleStream(
	ctx context.Context,
	spec fileSpec,
	upstream Upstream,
	part string,
	contributions map[string]int64,
) (int64, Outcome) {
	handle, outcome := e.open(ctx, http.MethodGet, upstream.Base+spec.key.path, nil)
	if outcome != "" {
		return 0, outcome
	}
	defer handle.close()
	response := handle.response
	if response.StatusCode != http.StatusOK {
		return 0, OutcomeHTTPStatus
	}
	if spec.size > 0 && response.ContentLength >= 0 && response.ContentLength != spec.size {
		return 0, OutcomeNetwork
	}
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		e.log("warning", "relay staging open failed", map[string]any{"item": spec.name, "error": err.Error()})
		return 0, OutcomeNetwork
	}
	hasher := sha256.New()
	received, outcome := e.readBody(ctx, handle, io.MultiWriter(out, hasher), spec.size, func(count int64) {
		e.tracker.add(spec.key, spec.name, upstream.Key, count)
	})
	if closeErr := out.Close(); closeErr != nil && outcome == "" {
		outcome = OutcomeNetwork
	}
	if outcome != "" {
		return received, outcome
	}
	if spec.size > 0 && received != spec.size {
		return received, OutcomeNetwork
	}
	if spec.digest != nil && subtle.ConstantTimeCompare(hasher.Sum(nil), spec.digest) != 1 {
		return received, OutcomeChecksumMismatch
	}
	if spec.size == 0 {
		// 未登记且 HEAD 没给出大小的文件：以实际收到的字节数作为交付大小。
		e.tracker.addTotal(received)
	}
	contributions[upstream.Key] += received
	return received, ""
}

// upstreamHandle 持有一个已返回响应头的上游响应及其取消函数。
type upstreamHandle struct {
	response *http.Response
	cancel   context.CancelFunc
}

func (h *upstreamHandle) close() {
	h.cancel()
	if h.response != nil && h.response.Body != nil {
		// 关闭失败意味着连接已断，没有可恢复的动作。
		_ = h.response.Body.Close()
	}
}

type doResult struct {
	response *http.Response
	err      error
}

// open 发起请求并在连接超时内等待响应头；只在这里拼上游 URL 之外的任何东西都不会被接受。
func (e *engine) open(
	ctx context.Context,
	method string,
	target string,
	headers map[string]string,
) (*upstreamHandle, Outcome) {
	if _, err := validateHTTPSURL(target); err != nil {
		return nil, OutcomeNetwork
	}
	requestCtx, cancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(requestCtx, method, target, nil)
	if err != nil {
		cancel()
		return nil, OutcomeNetwork
	}
	request.Header.Set("Accept-Encoding", "identity")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	results := make(chan doResult, 1)
	go func() {
		response, doErr := e.client.Do(request)
		results <- doResult{response: response, err: doErr}
	}()
	connectTimer := e.timers(e.cfg.ConnectTimeout)
	var (
		result  doResult
		outcome Outcome
	)
	select {
	case result = <-results:
		stopAndDrainTimer(connectTimer)
		switch {
		case ctx.Err() != nil:
			outcome = OutcomeCancelled
		case result.err != nil:
			outcome = OutcomeNetwork
		case result.response == nil:
			outcome = OutcomeNetwork
		}
	case <-ctx.Done():
		stopAndDrainTimer(connectTimer)
		cancel()
		result = <-results
		outcome = OutcomeCancelled
	case <-connectTimer.C():
		cancel()
		result = <-results
		outcome = OutcomeConnectTimeout
		if ctx.Err() != nil {
			outcome = OutcomeCancelled
		}
	}
	if outcome != "" {
		if result.response != nil && result.response.Body != nil {
			_ = result.response.Body.Close()
		}
		cancel()
		return nil, outcome
	}
	if err := validateFinalURL(result.response); err != nil {
		_ = result.response.Body.Close()
		cancel()
		return nil, OutcomeNetwork
	}
	return &upstreamHandle{response: result.response, cancel: cancel}, ""
}

func validateFinalURL(response *http.Response) error {
	if response.Request == nil || response.Request.URL == nil {
		return errURLPolicy
	}
	_, err := validateHTTPSURL(response.Request.URL.String())
	return err
}

type bodyChunk struct {
	data []byte
	err  error
}

// pumpBody 在独立 goroutine 里读响应体，让读循环能同时等待失速定时器与取消；
// ctx 结束或读到错误 / EOF 时退出。
func pumpBody(ctx context.Context, body io.Reader, chunks chan<- bodyChunk, done chan<- struct{}) {
	defer close(done)
	buffer := make([]byte, 64*1024)
	for {
		n, err := body.Read(buffer)
		if n == 0 && err == nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		chunk := bodyChunk{err: err}
		if n > 0 {
			chunk.data = append([]byte(nil), buffer[:n]...)
		}
		select {
		case chunks <- chunk:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// readBody 把响应体写入 writer，读空闲超过 StallTimeout 判失速，expected 大于零时超量即失败。
func (e *engine) readBody(
	ctx context.Context,
	handle *upstreamHandle,
	writer io.Writer,
	expected int64,
	onBytes func(count int64),
) (int64, Outcome) {
	requestCtx := handle.response.Request.Context()
	chunks := make(chan bodyChunk)
	pumpDone := make(chan struct{})
	go pumpBody(requestCtx, handle.response.Body, chunks, pumpDone)
	stallTimer := e.timers(e.cfg.StallTimeout)

	var (
		received int64
		outcome  Outcome
	)
loop:
	for {
		select {
		case chunk := <-chunks:
			stopAndDrainTimer(stallTimer)
			if len(chunk.data) > 0 {
				count := int64(len(chunk.data))
				if expected > 0 && received+count > expected {
					outcome = OutcomeNetwork
					break loop
				}
				if _, err := writer.Write(chunk.data); err != nil {
					outcome = OutcomeNetwork
					break loop
				}
				received += count
				onBytes(count)
			}
			if chunk.err != nil {
				if !errors.Is(chunk.err, io.EOF) {
					outcome = OutcomeNetwork
				}
				break loop
			}
			if ctx.Err() != nil {
				outcome = OutcomeCancelled
				break loop
			}
			stallTimer.Reset(e.cfg.StallTimeout)
		case <-stallTimer.C():
			outcome = OutcomeReadTimeout
			if ctx.Err() != nil {
				outcome = OutcomeCancelled
			}
			break loop
		case <-ctx.Done():
			outcome = OutcomeCancelled
			break loop
		}
	}
	stopAndDrainTimer(stallTimer)
	handle.cancel()
	<-pumpDone
	return received, outcome
}
