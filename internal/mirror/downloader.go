package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

const (
	defaultConnectTimeout   = 15 * time.Second
	defaultReadTimeout      = 30 * time.Second
	defaultProgressInterval = 200 * time.Millisecond
	cleanupTimeout          = 5 * time.Second
)

const (
	// FailureInvalidRequest 表示下载请求的固定字段无效。
	FailureInvalidRequest FailureKind = "invalid_request"
	// FailureURLPolicy 表示 URL 不符合绝对 HTTPS 策略。
	FailureURLPolicy FailureKind = "url_policy"
	// FailureRedirectDowngrade 表示 HTTPS 重定向尝试降级为 HTTP。
	FailureRedirectDowngrade FailureKind = "redirect_downgrade"
	// FailureConnectTimeout 表示请求在响应头返回前超时。
	FailureConnectTimeout FailureKind = "connect_timeout"
	// FailureReadTimeout 表示响应体网络读取空闲超时。
	FailureReadTimeout FailureKind = "read_timeout"
	// FailureNetwork 表示未细分的网络传输故障。
	FailureNetwork FailureKind = "network"
	// FailureHTTPStatus 表示响应状态不是 200。
	FailureHTTPStatus FailureKind = "http_status"
	// FailureSizeMismatch 表示制品大小与预期不一致。
	FailureSizeMismatch FailureKind = "size_mismatch"
	// FailureChecksumMismatch 表示制品摘要与预期不一致。
	FailureChecksumMismatch FailureKind = "checksum_mismatch"
	// FailureCancelled 表示调用方取消了下载。
	FailureCancelled FailureKind = "cancelled"
	// FailureProgress 表示同步进度回调失败。
	FailureProgress FailureKind = "progress"
	// FailureDestinationOccupied 表示最终目标已被其他对象占用。
	FailureDestinationOccupied FailureKind = "destination_occupied"
	// FailureFilesystem 表示本地文件会话操作失败。
	FailureFilesystem FailureKind = "filesystem"
)

// DownloadRequest 描述单次下载已冻结的 URL、文件身份和完整性预期。
type DownloadRequest struct {
	URL            string
	FileName       string
	ExpectedSize   int64
	ExpectedSHA256 string
	Progress       ProgressFunc
}

// DownloadProgress 描述已确认完整写入并纳入摘要的字节进度。
type DownloadProgress struct {
	Received int64
	Total    int64
	Percent  float64
}

// ProgressFunc 同步接收单调下载进度。
type ProgressFunc func(progress DownloadProgress) error

// DownloadResult 描述已发布或已发生发布事实的制品。
type DownloadResult struct {
	Path   string
	Size   int64
	SHA256 string
}

// DownloadFailure 保存稳定分类、发布事实以及主错误和清理错误链。
type DownloadFailure struct {
	Kind       FailureKind
	StatusCode int
	Published  bool
	Err        error
	CleanupErr error
}

// Error 返回不含 URL 或外部错误原文的稳定文本。
func (e *DownloadFailure) Error() string {
	if e == nil {
		return "download failed"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("download failed: %s status %d", e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("download failed: %s", e.Kind)
}

// Unwrap 按主错误、清理错误的顺序保留完整类型链。
func (e *DownloadFailure) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.Err != nil {
		causes = append(causes, e.Err)
	}
	if e.CleanupErr != nil {
		causes = append(causes, e.CleanupErr)
	}
	return causes
}

type downloaderSafeError struct {
	message string
	cause   error
}

func (e *downloaderSafeError) Error() string {
	return e.message
}

func (e *downloaderSafeError) Unwrap() error {
	return e.cause
}

func safeExternalError(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &downloaderSafeError{message: message, cause: cause}
}

type httpClient interface {
	Do(request *http.Request) (*http.Response, error)
}

type timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(delay time.Duration) bool
}

type timerFactory func(delay time.Duration) timer

type downloadSession interface {
	Write(p []byte) (int, error)
	Path() string
	PartPath() string
	PublishNoReplace(ctx context.Context) (filesystem.PublishResult, error)
	Abort(ctx context.Context) (filesystem.AbortResult, error)
}

type sessionFactory interface {
	Begin(ctx context.Context, name string) (downloadSession, error)
}

type cleanupContextFactory func(
	operationCtx context.Context,
) (context.Context, context.CancelFunc)

type filesystemSessions struct {
	files *filesystem.DownloadFiles
}

func (f filesystemSessions) Begin(
	ctx context.Context,
	name string,
) (downloadSession, error) {
	return f.files.Begin(ctx, name)
}

type runtimeTimer struct {
	timer *time.Timer
}

func (t *runtimeTimer) C() <-chan time.Time {
	return t.timer.C
}

func (t *runtimeTimer) Stop() bool {
	return t.timer.Stop()
}

func (t *runtimeTimer) Reset(delay time.Duration) bool {
	return t.timer.Reset(delay)
}

type downloaderOptions struct {
	connectTimeout     time.Duration
	readTimeout        time.Duration
	progressInterval   time.Duration
	timeoutsConfigured bool
	progressConfigured bool
}

// DownloaderOption 配置 Downloader 的有限时间参数。
type DownloaderOption func(*downloaderOptions) error

// WithDownloaderTimeouts 设置连接阶段与读取空闲超时。
func WithDownloaderTimeouts(
	connectTimeout time.Duration,
	readTimeout time.Duration,
) DownloaderOption {
	return func(options *downloaderOptions) error {
		if options == nil || options.timeoutsConfigured ||
			connectTimeout <= 0 || readTimeout <= 0 {
			return ErrInvalidDownloaderOption
		}
		options.connectTimeout = connectTimeout
		options.readTimeout = readTimeout
		options.timeoutsConfigured = true
		return nil
	}
}

// WithDownloaderProgressInterval 设置非终点进度的最小报告间隔。
func WithDownloaderProgressInterval(interval time.Duration) DownloaderOption {
	return func(options *downloaderOptions) error {
		if options == nil || options.progressConfigured || interval <= 0 {
			return ErrInvalidDownloaderOption
		}
		options.progressInterval = interval
		options.progressConfigured = true
		return nil
	}
}

// ErrInvalidDownloaderOption 表示 Downloader option 或私有依赖无效。
var ErrInvalidDownloaderOption = errors.New("downloader option is invalid")

func resolveDownloaderOptions(options []DownloaderOption) (downloaderOptions, error) {
	resolved := downloaderOptions{
		connectTimeout:   defaultConnectTimeout,
		readTimeout:      defaultReadTimeout,
		progressInterval: defaultProgressInterval,
	}
	for _, option := range options {
		if option == nil {
			return downloaderOptions{}, fmt.Errorf(
				"configure downloader: %w",
				ErrInvalidDownloaderOption,
			)
		}
		if err := option(&resolved); err != nil {
			return downloaderOptions{}, fmt.Errorf("configure downloader: %w", err)
		}
	}
	return resolved, nil
}

type downloaderDependencies struct {
	sessions sessionFactory
	client   httpClient
	clock    func() time.Time
	timers   timerFactory
	cleanup  cleanupContextFactory
}

// Downloader 执行一个已确定资源的一次流式 HTTPS 下载。
type Downloader struct {
	layout           *config.Layout
	sessions         sessionFactory
	client           httpClient
	clock            func() time.Time
	timers           timerFactory
	cleanup          cleanupContextFactory
	connectTimeout   time.Duration
	readTimeout      time.Duration
	progressInterval time.Duration
}

// NewDownloader 创建构造后只读、可并发调用的 Downloader。
func NewDownloader(
	layout *config.Layout,
	options ...DownloaderOption,
) (*Downloader, error) {
	if layout == nil {
		return nil, fmt.Errorf("configure downloader layout: %w", ErrInvalidDownloaderOption)
	}
	resolved, err := resolveDownloaderOptions(options)
	if err != nil {
		return nil, err
	}
	files, err := filesystem.NewDownloadFiles(layout)
	if err != nil {
		return nil, safeExternalError("create download files failed", err)
	}
	return newDownloaderWithDependencies(
		layout,
		resolved,
		downloaderDependencies{
			sessions: filesystemSessions{files: files},
			client:   newDefaultHTTPClient(),
			clock:    time.Now,
			timers: func(delay time.Duration) timer {
				return &runtimeTimer{timer: time.NewTimer(delay)}
			},
			cleanup: productionCleanupContext,
		},
	)
}

func productionCleanupContext(
	operationCtx context.Context,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		context.WithoutCancel(operationCtx),
		cleanupTimeout,
	)
}

func newDownloaderWithDependencies(
	layout *config.Layout,
	options downloaderOptions,
	dependencies downloaderDependencies,
) (*Downloader, error) {
	if layout == nil ||
		dependencies.sessions == nil || dependencies.client == nil ||
		dependencies.clock == nil || dependencies.timers == nil ||
		dependencies.cleanup == nil ||
		options.connectTimeout <= 0 || options.readTimeout <= 0 ||
		options.progressInterval <= 0 {
		return nil, fmt.Errorf("configure downloader dependencies: %w", ErrInvalidDownloaderOption)
	}
	return &Downloader{
		layout:           layout,
		sessions:         dependencies.sessions,
		client:           dependencies.client,
		clock:            dependencies.clock,
		timers:           dependencies.timers,
		cleanup:          dependencies.cleanup,
		connectTimeout:   options.connectTimeout,
		readTimeout:      options.readTimeout,
		progressInterval: options.progressInterval,
	}, nil
}

type validatedRequest struct {
	url            *url.URL
	fileName       string
	expectedSize   int64
	expectedSHA256 string
	expectedDigest [32]byte
	progress       ProgressFunc
}

func (d *Downloader) validateRequest(
	ctx context.Context,
	request DownloadRequest,
) (validatedRequest, *DownloadFailure) {
	if ctx == nil {
		return validatedRequest{}, newDownloadFailure(
			FailureInvalidRequest,
			0,
			errors.New("download context is nil"),
		)
	}
	parsed, err := validateHTTPSURL(request.URL)
	if err != nil {
		return validatedRequest{}, newDownloadFailure(FailureURLPolicy, 0, err)
	}
	if _, err := d.layout.DownloadFile(request.FileName); err != nil {
		return validatedRequest{}, newDownloadFailure(FailureInvalidRequest, 0, err)
	}
	if _, err := d.layout.DownloadPartFile(request.FileName); err != nil {
		return validatedRequest{}, newDownloadFailure(FailureInvalidRequest, 0, err)
	}
	if request.ExpectedSize <= 0 {
		return validatedRequest{}, newDownloadFailure(
			FailureInvalidRequest,
			0,
			errors.New("download size is invalid"),
		)
	}
	digestBytes, err := hex.DecodeString(request.ExpectedSHA256)
	if err != nil || len(digestBytes) != sha256.Size {
		return validatedRequest{}, newDownloadFailure(
			FailureInvalidRequest,
			0,
			errors.New("download checksum is invalid"),
		)
	}
	var digest [32]byte
	copy(digest[:], digestBytes)
	return validatedRequest{
		url:            parsed,
		fileName:       request.FileName,
		expectedSize:   request.ExpectedSize,
		expectedSHA256: hex.EncodeToString(digestBytes),
		expectedDigest: digest,
		progress:       request.Progress,
	}, nil
}

func newDownloadFailure(
	kind FailureKind,
	statusCode int,
	cause error,
) *DownloadFailure {
	return &DownloadFailure{
		Kind:       kind,
		StatusCode: statusCode,
		Err:        safeExternalError("download operation failed", cause),
	}
}

func validateHTTPSURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() ||
		!strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("download URL violates HTTPS policy")
	}
	return parsed, nil
}
