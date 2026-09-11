package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultProbeWindowBytes 是 C16 规定的探针 Range 窗口（256 KiB）。
	DefaultProbeWindowBytes int64 = 256 * 1024

	defaultProbeBudget = 3 * time.Second
	// minProbeElapsed 是吞吐计算的最小时间基数：本机时钟分辨率有限，读完窗口
	// 与收到响应头可能落在同一个时钟刻度上，不能让一次极快的探测除以零。
	minProbeElapsed = time.Millisecond
)

var (
	// ErrInvalidProbeOption 表示 NewProber 的 option 非法或重复。
	ErrInvalidProbeOption = errors.New("mirror probe option is invalid")
	// ErrInvalidProbeRequest 表示 Probe 的 Plan 或 ProbeTarget 参数非法。
	ErrInvalidProbeRequest = errors.New("mirror probe request is invalid")

	errProbeBudgetExceeded = errors.New("probe budget exceeded before response headers")
	errProbeCancelled      = errors.New("probe request cancelled")
	errProbeHTTPStatus     = errors.New("probe response status is not successful")
	errProbeRedirectPolicy = errors.New("probe redirect violates download policy")
	errProbeNoResponse     = errors.New("http client returned no response")
)

// ProbeTarget 描述一个 Kind 的探针：Path 拼在每个源的 base（package-index 用 packages 前缀）之后。
type ProbeTarget struct {
	Kind Kind
	// Path 例："0.12.3/uv-x86_64-pc-windows-msvc.zip"；git 为 "info/refs?service=git-upload-pack"。
	Path string
	// WindowBytes 是 Range 窗口；为 0 时不带 Range、只测 TTFB，收到响应头即完成。
	WindowBytes int64
}

// ProbeResult 是单个源的探测结果；Err 非空时 OK 为 false，其余数值为零。
type ProbeResult struct {
	Source         Source
	OK             bool
	TTFB           time.Duration
	BytesPerSecond int64
	Bytes          int64
	// AcceptRanges 表示返回 206 或响应头 Accept-Ranges: bytes。
	AcceptRanges bool
	Err          error
}

// ProbeOption 配置构造后只读的 Prober。
type ProbeOption func(*probeOptions) error

type probeOptions struct {
	budget time.Duration
	client httpClient
	clock  func() time.Time

	budgetSet bool
	clientSet bool
	clockSet  bool
}

// Prober 并行探测一个 Plan 内全部源的首字节延迟与窗口吞吐。
// 同一 Prober 可被多个 goroutine 并发调用 Probe。
type Prober struct {
	budget time.Duration
	client httpClient
	clock  func() time.Time
}

// WithProbeBudget 设置一次 Probe 的总预算，默认 3s。
func WithProbeBudget(budget time.Duration) ProbeOption {
	return func(options *probeOptions) error {
		if options == nil || budget <= 0 || options.budgetSet {
			return fmt.Errorf("%w: budget", ErrInvalidProbeOption)
		}
		options.budget = budget
		options.budgetSet = true
		return nil
	}
}

// WithProbeClient 注入 HTTP 客户端；测试用 httptest，生产默认沿用下载器的重定向策略。
func WithProbeClient(client httpClient) ProbeOption {
	return func(options *probeOptions) error {
		if options == nil || client == nil || options.clientSet {
			return fmt.Errorf("%w: client", ErrInvalidProbeOption)
		}
		options.client = client
		options.clientSet = true
		return nil
	}
}

// WithProbeClock 注入计时使用的时钟。
func WithProbeClock(clock func() time.Time) ProbeOption {
	return func(options *probeOptions) error {
		if options == nil || clock == nil || options.clockSet {
			return fmt.Errorf("%w: clock", ErrInvalidProbeOption)
		}
		options.clock = clock
		options.clockSet = true
		return nil
	}
}

// NewProber 构造只读 Prober；未注入的依赖使用生产默认值。
func NewProber(options ...ProbeOption) (*Prober, error) {
	configured := probeOptions{
		budget: defaultProbeBudget,
		clock:  time.Now,
	}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil option at index %d", ErrInvalidProbeOption, index)
		}
		if err := option(&configured); err != nil {
			return nil, err
		}
	}
	if configured.client == nil {
		configured.client = newDefaultHTTPClient()
	}
	return &Prober{
		budget: configured.budget,
		client: configured.client,
		clock:  configured.clock,
	}, nil
}

type indexedProbeResult struct {
	index  int
	result ProbeResult
}

// Probe 并行探测 plan 内全部源，预算到期即收口；永不返回网络错误（结果写进 ProbeResult.Err）。
// 只有 ctx 取消或参数非法才返回 error。report 可为 nil，每个源完成时在调用方 goroutine
// 上同步回调一次，回调之间不会并发。返回的切片按 plan 顺序排列。
func (p *Prober) Probe(
	ctx context.Context,
	plan Plan,
	target ProbeTarget,
	report func(ProbeResult),
) ([]ProbeResult, error) {
	if err := validateProbeRequest(p, ctx, plan, target); err != nil {
		return nil, err
	}
	urls, err := probeURLs(plan, target)
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, newSafeError(safeErrorCancellation, ctxErr)
	}

	budgetCtx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	outcomes := make(chan indexedProbeResult, len(plan.sources))
	var workers sync.WaitGroup
	for index, source := range plan.sources {
		workers.Add(1)
		// 每个源一个 goroutine；退出条件：请求完成、预算到期或 ctx 取消都会让
		// probeSource 返回，结果经带缓冲的 outcomes 发出后 goroutine 立即结束。
		go func(index int, source Source, url string) {
			defer workers.Done()
			outcomes <- indexedProbeResult{
				index:  index,
				result: p.probeSource(ctx, budgetCtx, source, url, target.WindowBytes),
			}
		}(index, source, urls[index])
	}

	results := make([]ProbeResult, len(plan.sources))
	for received := 0; received < len(plan.sources); received++ {
		select {
		case outcome := <-outcomes:
			if ctxErr := ctx.Err(); ctxErr != nil {
				cancel()
				workers.Wait()
				return nil, newSafeError(safeErrorCancellation, ctxErr)
			}
			results[outcome.index] = outcome.result
			if report != nil {
				report(outcome.result)
			}
		case <-ctx.Done():
			cancel()
			workers.Wait()
			return nil, newSafeError(safeErrorCancellation, ctx.Err())
		}
	}
	workers.Wait()
	return results, nil
}

func validateProbeRequest(
	prober *Prober,
	ctx context.Context,
	plan Plan,
	target ProbeTarget,
) error {
	if prober == nil || prober.client == nil || prober.clock == nil || prober.budget <= 0 {
		return fmt.Errorf("%w: prober", ErrInvalidProbeRequest)
	}
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidProbeRequest)
	}
	if !validPlan(plan) || plan.offline || plan.kind != target.Kind {
		return fmt.Errorf("%w: plan", ErrInvalidProbeRequest)
	}
	if !validProbePath(target.Path) || target.WindowBytes < 0 {
		return fmt.Errorf("%w: target", ErrInvalidProbeRequest)
	}
	return nil
}

func validProbePath(path string) bool {
	if path == "" {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] <= ' ' || path[i] == 0x7f {
			return false
		}
	}
	return true
}

// probeURLs 在启动任何 goroutine 之前为全部源拼好探针地址；任一失败都是参数错误。
func probeURLs(plan Plan, target ProbeTarget) ([]string, error) {
	urls := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		base := source.baseURL
		if rewrite, ok := source.PackageIndexRewrite(); ok {
			base = rewrite.PackagesBase()
		}
		joined := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(target.Path, "/")
		if _, err := validateHTTPSURL(joined); err != nil {
			return nil, fmt.Errorf("%w: probe url for %s", ErrInvalidProbeRequest, source.key)
		}
		urls = append(urls, joined)
	}
	return urls, nil
}

// probeSource 对单个源执行一次探针请求。失败只写进 ProbeResult.Err，绝不 panic 或返回 error。
func (p *Prober) probeSource(
	operationCtx context.Context,
	budgetCtx context.Context,
	source Source,
	url string,
	window int64,
) ProbeResult {
	result := ProbeResult{Source: source}
	request, err := http.NewRequestWithContext(budgetCtx, http.MethodGet, url, nil)
	if err != nil {
		result.Err = safeExternalError("probe request build failed", err)
		return result
	}
	request.Header.Set("Accept-Encoding", "identity")
	if window > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", window-1))
	}

	startedAt := p.clock()
	response, err := p.client.Do(request)
	if err != nil {
		result.Err = classifyProbeTransportError(operationCtx, budgetCtx, err)
		return result
	}
	headersAt := p.clock()
	if response == nil {
		result.Err = errProbeNoResponse
		return result
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	// 探针只关心计时，响应体的关闭错误既不影响排序也无法补救，因此不上报。
	defer func() { _ = response.Body.Close() }()

	if kind, finalErr := validateFinalResponseURL(response); finalErr != nil {
		result.Err = fmt.Errorf("%w: %s", errProbeRedirectPolicy, kind)
		return result
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Err = fmt.Errorf("%w: %d", errProbeHTTPStatus, response.StatusCode)
		return result
	}

	ttfb := headersAt.Sub(startedAt)
	if ttfb < 0 {
		ttfb = 0
	}
	acceptRanges := response.StatusCode == http.StatusPartialContent ||
		strings.EqualFold(strings.TrimSpace(response.Header.Get("Accept-Ranges")), "bytes")
	if window == 0 {
		result.OK = true
		result.TTFB = ttfb
		result.AcceptRanges = acceptRanges
		return result
	}

	read, readErr := io.CopyN(io.Discard, response.Body, window)
	finishedAt := p.clock()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		if operationCtx.Err() != nil {
			result.Err = errors.Join(errProbeCancelled, operationCtx.Err())
			return result
		}
		// 预算到期时正在读的源按已读字节计算吞吐并记 OK（C16 第 2 条的收口语义）；
		// 其它读取错误（连接被重置等）才算探针失败。
		if budgetCtx.Err() == nil {
			result.Err = safeExternalError("probe response read failed", readErr)
			return result
		}
	}
	result.OK = true
	result.TTFB = ttfb
	result.AcceptRanges = acceptRanges
	result.Bytes = read
	result.BytesPerSecond = probeThroughput(read, finishedAt.Sub(headersAt))
	return result
}

func classifyProbeTransportError(
	operationCtx context.Context,
	budgetCtx context.Context,
	err error,
) error {
	sanitized := sanitizeTransportError(err)
	if operationCtx.Err() != nil {
		return errors.Join(errProbeCancelled, operationCtx.Err(), sanitized)
	}
	if budgetCtx.Err() != nil {
		return errors.Join(errProbeBudgetExceeded, sanitized)
	}
	var redirect *redirectError
	if errors.As(err, &redirect) {
		return fmt.Errorf("%w: %s", errProbeRedirectPolicy, redirect.kind)
	}
	return sanitized
}

// probeThroughput 按首字节之后读到的字节与经过的时间计算吞吐；时间不足一个最小基数时按基数算。
func probeThroughput(bytes int64, elapsed time.Duration) int64 {
	if bytes <= 0 {
		return 0
	}
	if elapsed < minProbeElapsed {
		elapsed = minProbeElapsed
	}
	return int64(float64(bytes) / elapsed.Seconds())
}
