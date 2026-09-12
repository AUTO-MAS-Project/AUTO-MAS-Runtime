package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	stagedContentType = "application/octet-stream"
)

// fileFetcher 把一个路由内的文件完整取到暂存目录并返回其路径；实现方负责多源回退与校验。
type fileFetcher interface {
	fetch(ctx context.Context, route Route, path string) (string, error)
}

// internals 是只供同包测试替换的私有依赖；生产路径由 Start 统一填默认值。
type internals struct {
	timers  timerFactory
	fetcher fileFetcher
	// slotWaiting 非 nil 时，每有一次取回开始等待文件槽位就非阻塞地发一次信号。
	slotWaiting chan struct{}
}

// fileEntry 是同一文件的共享取回结果：done 关闭后 path / err 只读。
type fileEntry struct {
	done chan struct{}
	path string
	err  error
}

// Server 是运行中的回环中继。
type Server struct {
	cfg     Config
	deps    Deps
	baseURL string

	listener net.Listener
	http     *http.Server
	fetcher  fileFetcher
	staging  *staging

	// fetchCtx 在 ctx 取消、Close 或进度回调失败时取消，所有取回都挂在它下面。
	fetchCtx    context.Context
	cancelFetch context.CancelFunc

	mu    sync.Mutex // 保护 files。
	files map[string]*fileEntry

	inflight   atomic.Int64 // 正在执行的 HTTP handler 数，供 Close 收口断言。
	stopped    atomic.Bool  // 进度回调失败后置位，后续请求一律 502。
	background sync.WaitGroup
	closed     chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

// Start 绑定 127.0.0.1:0 并开始服务；ctx 取消即关闭中继。
func Start(ctx context.Context, cfg Config, deps Deps) (*Server, error) {
	return start(ctx, cfg, deps, internals{})
}

func start(
	ctx context.Context,
	cfg Config,
	deps Deps,
	private internals,
) (*Server, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidConfig)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	deps = normalizeDeps(deps)
	if private.timers == nil {
		private.timers = newRuntimeTimer
	}
	if err := os.MkdirAll(normalized.StagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("create relay staging dir: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen relay loopback: %w", err)
	}
	fetchCtx, cancelFetch := context.WithCancel(ctx)
	server := &Server{
		cfg:         normalized,
		deps:        deps,
		baseURL:     "http://" + listener.Addr().String(),
		listener:    listener,
		staging:     newStaging(normalized.StagingDir),
		fetchCtx:    fetchCtx,
		cancelFetch: cancelFetch,
		files:       make(map[string]*fileEntry),
		closed:      make(chan struct{}),
	}
	server.fetcher = private.fetcher
	if server.fetcher == nil {
		server.fetcher = newEngine(normalized, deps, private, server.staging, server.baseURL, server.stop)
	}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          log.New(loggerWriter{logger: deps.Logger}, "", 0),
	}
	server.background.Add(2)
	go server.serve()
	go server.watch(ctx)
	return server, nil
}

// serve 在 Close 关闭监听后退出。
func (s *Server) serve() {
	defer s.background.Done()
	// Serve 只在监听被关闭时返回，返回值恒为 ErrServerClosed 或监听错误，无需上报。
	_ = s.http.Serve(s.listener)
}

// watch 在 ctx 取消时关闭中继；Close 先于 ctx 取消时随 closed 退出。
func (s *Server) watch(ctx context.Context) {
	defer s.background.Done()
	select {
	case <-ctx.Done():
		// 关闭错误由 Close 的调用方读取；这里只保证 ctx 取消一定触发收口。
		go func() { _ = s.Close() }()
	case <-s.closed:
	}
}

// stop 在进度回调失败时把中继置为停止：进行中取回被取消，后续请求一律 502。
func (s *Server) stop() {
	s.stopped.Store(true)
	s.cancelFetch()
}

// BaseURL 返回 "http://127.0.0.1:<port>"。
func (s *Server) BaseURL() string { return s.baseURL }

// Register 追加已知制品（幂等）。
func (s *Server) Register(items []Item) {
	if registrar, ok := s.fetcher.(itemRegistrar); ok {
		registrar.register(items)
	}
}

// SetUpstreams 在运行期替换某条路由的上游顺序（受监督时后台测速完成后调用）。
// 已因两次哈希不符被剔除的源不会被重新加回；非法路由或上游返回错误且不改变现有顺序。
func (s *Server) SetUpstreams(route Route, upstreams []Upstream) error {
	if !route.Valid() {
		return fmt.Errorf("%w: route %q", ErrInvalidConfig, route)
	}
	normalized := make([]Upstream, 0, len(upstreams))
	for _, upstream := range upstreams {
		entry, err := normalizeUpstream(upstream)
		if err != nil {
			return err
		}
		normalized = append(normalized, entry)
	}
	if setter, ok := s.fetcher.(upstreamSetter); ok {
		setter.setUpstreams(route, normalized)
	}
	return nil
}

// Summary 返回供 result.details.relay 使用的汇总快照。
func (s *Server) Summary() Summary {
	if summarizer, ok := s.fetcher.(summarizer); ok {
		return summarizer.summary()
	}
	return Summary{BySource: map[string]int64{}}
}

// Close 停止接受连接、取消进行中取回、有界等待全部 goroutine 退出并清空暂存目录。
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.cancelFetch()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		shutdownErr := s.http.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			// 优雅关闭超时后强制断开残余连接，保证收口有界。
			shutdownErr = errors.Join(shutdownErr, s.http.Close())
		}
		waitErr := waitBounded(&s.background, closeTimeout)
		cleanErr := s.staging.clean()
		s.closeErr = errors.Join(shutdownErr, waitErr, cleanErr)
	})
	return s.closeErr
}

func waitBounded(group *sync.WaitGroup, limit time.Duration) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	bound := time.NewTimer(limit)
	defer bound.Stop()
	select {
	case <-done:
		return nil
	case <-bound.C:
		return errors.New("relay goroutines did not exit before close deadline")
	}
}

// ServeHTTP 只做路由校验与分发；业务错误一律以状态码表达，不写任何标准输出。
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	resolved := resolveRoute(request)
	if resolved.status != 0 {
		writer.WriteHeader(resolved.status)
		return
	}
	if s.stopped.Load() || s.fetchCtx.Err() != nil {
		writeStatus(writer, http.StatusBadGateway, "relay is stopped")
		return
	}
	if resolved.route == RouteSimple {
		s.serveSimple(writer, request, resolved.path)
		return
	}
	s.serveFile(writer, request, resolved.route, resolved.path)
}

func (s *Server) serveFile(
	writer http.ResponseWriter,
	request *http.Request,
	route Route,
	path string,
) {
	staged, err := s.awaitFile(request.Context(), route, path)
	if err != nil {
		if request.Context().Err() != nil {
			return
		}
		writeStatus(writer, http.StatusBadGateway, "relay: all sources failed")
		return
	}
	file, err := os.Open(staged)
	if err != nil {
		writeStatus(writer, http.StatusBadGateway, "relay: staged file unavailable")
		return
	}
	defer func() {
		// 只读句柄关闭失败没有可恢复的动作，响应已经写出。
		_ = file.Close()
	}()
	writer.Header().Set("Content-Type", stagedContentType)
	http.ServeContent(writer, request, "", time.Time{}, file)
}

// awaitFile 让同一文件的并发请求共享一次取回：第一个到达的请求启动取回，其余等待同一结果。
// 取回挂在 fetchCtx 下而不是请求 ctx 下，请求方离开不会打断其他等待者。
func (s *Server) awaitFile(ctx context.Context, route Route, path string) (string, error) {
	key := route.String() + "/" + path
	s.mu.Lock()
	entry, ok := s.files[key]
	if !ok {
		entry = &fileEntry{done: make(chan struct{})}
		s.files[key] = entry
		s.background.Add(1)
		go s.runFetch(key, route, path, entry)
	}
	s.mu.Unlock()
	select {
	case <-entry.done:
		return entry.path, entry.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.fetchCtx.Done():
		return "", s.fetchCtx.Err()
	}
}

// runFetch 是取回 goroutine：失败结果不缓存，让后续请求可以重试；成功文件留在暂存目录直到 Close。
func (s *Server) runFetch(key string, route Route, path string, entry *fileEntry) {
	defer s.background.Done()
	entry.path, entry.err = s.fetcher.fetch(s.fetchCtx, route, path)
	if entry.err != nil {
		s.mu.Lock()
		delete(s.files, key)
		s.mu.Unlock()
	}
	close(entry.done)
}

// serveSimple 把请求 ctx 与中继 ctx 合并后交给引擎：请求方离开或 Close 都能打断索引取回。
func (s *Server) serveSimple(writer http.ResponseWriter, request *http.Request, name string) {
	handler, ok := s.fetcher.(simpleFetcher)
	if !ok {
		writeStatus(writer, http.StatusBadGateway, "relay: all sources failed")
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	stop := context.AfterFunc(s.fetchCtx, cancel)
	defer stop()
	handler.serveSimple(ctx, writer, request, name)
}

func writeStatus(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	// 响应体只是给 uv 日志看的提示，写失败意味着对端已断开，没有可恢复的动作。
	_, _ = writer.Write([]byte(message))
}

// itemRegistrar / summarizer / simpleFetcher 是取回引擎按需实现的可选能力。
type upstreamSetter interface {
	setUpstreams(route Route, upstreams []Upstream)
}

type itemRegistrar interface {
	register(items []Item)
}

type summarizer interface {
	summary() Summary
}

type simpleFetcher interface {
	serveSimple(ctx context.Context, writer http.ResponseWriter, request *http.Request, name string)
}

// loggerWriter 把 http.Server 的内部诊断转给注入的 Logger，避免标准库落到 stderr 之外的任何地方。
type loggerWriter struct {
	logger func(level, message string, fields map[string]any)
}

func (w loggerWriter) Write(p []byte) (int, error) {
	if w.logger != nil {
		w.logger("warning", strings.TrimSpace(string(p)), map[string]any{"component": "relay-http"})
	}
	return len(p), nil
}

// staging 记录中继自己创建的暂存文件，Close 时只删除这些文件，不做递归删除。
type staging struct {
	dir     string
	mu      sync.Mutex // 保护 created。
	created map[string]struct{}
}

func newStaging(dir string) *staging {
	return &staging{dir: dir, created: make(map[string]struct{})}
}

// pathFor 用路由与路径的摘要命名暂存文件，避免把上游路径中的任何字符带进本地文件名。
func (st *staging) pathFor(route Route, path string) string {
	digest := sha256.Sum256([]byte(route.String() + "/" + path))
	return filepath.Join(st.dir, hex.EncodeToString(digest[:16]))
}

func (st *staging) track(path string) {
	st.mu.Lock()
	st.created[path] = struct{}{}
	st.mu.Unlock()
}

func (st *staging) clean() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var errs []error
	for path := range st.created {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove relay staged file: %w", err))
		}
		delete(st.created, path)
	}
	// 目录本身只在为空时移除；里面若有不属于本中继的内容就留给 cleanup 处理。
	_ = os.Remove(st.dir)
	return errors.Join(errs...)
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.StagingDir == "" || !filepath.IsAbs(cfg.StagingDir) {
		return Config{}, fmt.Errorf("%w: staging dir must be absolute", ErrInvalidConfig)
	}
	if len(cfg.Upstreams) == 0 {
		return Config{}, fmt.Errorf("%w: upstreams are required", ErrInvalidConfig)
	}
	upstreams := make(map[Route][]Upstream, len(cfg.Upstreams))
	for route, list := range cfg.Upstreams {
		if !route.Valid() {
			return Config{}, fmt.Errorf("%w: route %q", ErrInvalidConfig, route)
		}
		normalized := make([]Upstream, 0, len(list))
		for _, upstream := range list {
			entry, err := normalizeUpstream(upstream)
			if err != nil {
				return Config{}, err
			}
			normalized = append(normalized, entry)
		}
		upstreams[route] = normalized
	}
	items := make([]Item, 0, len(cfg.Items))
	for _, item := range cfg.Items {
		if err := validateItem(item); err != nil {
			return Config{}, err
		}
		items = append(items, item)
	}
	normalized := Config{
		StagingDir:       filepath.Clean(cfg.StagingDir),
		Upstreams:        upstreams,
		Items:            items,
		ChunkThreshold:   cfg.ChunkThreshold,
		ChunkSize:        cfg.ChunkSize,
		MaxFiles:         cfg.MaxFiles,
		MaxChunksPerFile: cfg.MaxChunksPerFile,
		StallTimeout:     cfg.StallTimeout,
		ConnectTimeout:   cfg.ConnectTimeout,
	}
	if normalized.ChunkThreshold == 0 {
		normalized.ChunkThreshold = DefaultChunkThreshold
	}
	if normalized.ChunkSize == 0 {
		normalized.ChunkSize = DefaultChunkSize
	}
	if normalized.MaxFiles == 0 {
		normalized.MaxFiles = DefaultMaxFiles
	}
	if normalized.MaxChunksPerFile == 0 {
		normalized.MaxChunksPerFile = DefaultMaxChunksPerFile
	}
	if normalized.StallTimeout == 0 {
		normalized.StallTimeout = DefaultStallTimeout
	}
	if normalized.ConnectTimeout == 0 {
		normalized.ConnectTimeout = DefaultConnectTimeout
	}
	if normalized.ChunkThreshold < 0 || normalized.ChunkSize <= 0 ||
		normalized.MaxFiles <= 0 || normalized.MaxChunksPerFile <= 0 ||
		normalized.StallTimeout <= 0 || normalized.ConnectTimeout <= 0 {
		return Config{}, fmt.Errorf("%w: limits must be positive", ErrInvalidConfig)
	}
	return normalized, nil
}

func normalizeUpstream(upstream Upstream) (Upstream, error) {
	if upstream.Key == "" {
		return Upstream{}, fmt.Errorf("%w: upstream key is empty", ErrInvalidConfig)
	}
	if _, err := validateHTTPSURL(upstream.Base); err != nil {
		return Upstream{}, fmt.Errorf("%w: upstream %q base: %w", ErrInvalidConfig, upstream.Key, err)
	}
	if upstream.RewriteFrom != "" {
		if _, err := validateHTTPSURL(upstream.RewriteFrom); err != nil {
			return Upstream{}, fmt.Errorf("%w: upstream %q rewrite prefix: %w", ErrInvalidConfig, upstream.Key, err)
		}
	}
	normalized := upstream
	if !strings.HasSuffix(normalized.Base, "/") {
		normalized.Base += "/"
	}
	return normalized, nil
}

func validateItem(item Item) error {
	if item.Path == "" || strings.HasPrefix(item.Path, "/") || strings.HasSuffix(item.Path, "/") {
		return fmt.Errorf("%w: item path %q", ErrInvalidConfig, item.Path)
	}
	for _, segment := range strings.Split(item.Path, "/") {
		if !validSegment(segment) {
			return fmt.Errorf("%w: item path %q", ErrInvalidConfig, item.Path)
		}
	}
	if item.Size <= 0 {
		return fmt.Errorf("%w: item %q size", ErrInvalidConfig, item.Path)
	}
	digest, err := hex.DecodeString(item.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("%w: item %q sha256", ErrInvalidConfig, item.Path)
	}
	return nil
}

func normalizeDeps(deps Deps) Deps {
	if deps.Client == nil {
		deps.Client = newDefaultHTTPClient()
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	return deps
}
