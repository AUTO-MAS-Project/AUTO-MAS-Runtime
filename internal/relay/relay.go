// Package relay 在 127.0.0.1 上提供纯 HTTP 回环中继，让 uv 只从本机取文件：
// 逐文件多源回退、大文件分片并行、已知哈希先校验再交付，并把聚合进度同步回调出去。
//
// 中继只监听回环地址，上游 URL 只能由目录内源的 base 与请求路径拼出；
// 它不代理任何业务流量，也不跨操作缓存文件。
package relay

import (
	"errors"
	"net/http"
	"time"
)

const (
	// DefaultChunkThreshold 是启用分片取回的最小已知文件大小。
	DefaultChunkThreshold int64 = 8 << 20
	// DefaultChunkSize 是分片取回的单片大小。
	DefaultChunkSize int64 = 2 << 20
	// DefaultMaxFiles 是同时下载的文件数上限。
	DefaultMaxFiles = 6
	// DefaultMaxChunksPerFile 是单个文件并行取片的 worker 上限。
	DefaultMaxChunksPerFile = 4
	// DefaultStallTimeout 是响应体读空闲失速判定时间。
	DefaultStallTimeout = 15 * time.Second
	// DefaultConnectTimeout 是响应头返回前的连接超时。
	DefaultConnectTimeout = 15 * time.Second
)

const (
	progressInterval = 200 * time.Millisecond
	throughputWindow = time.Second
	closeTimeout     = 30 * time.Second
	maxFailures      = 32
)

// ErrInvalidConfig 表示 Config 或 Deps 无法用于启动中继。
var ErrInvalidConfig = errors.New("relay config is invalid")

// Item 描述锁内已知的制品；Path 是 PyPI 原路径（"ab/cd/<hash>/x.whl"），SHA256 为十六进制。
type Item struct {
	Path   string
	Size   int64
	SHA256 string
}

// Route 是中继的三个前缀之一。
type Route string

const (
	// RoutePackages 对应 /packages/<PyPI 原路径>。
	RoutePackages Route = "packages"
	// RouteSimple 对应 /simple/<包名>/。
	RouteSimple Route = "simple"
	// RoutePython 对应 /python/<tag>/<asset>。
	RoutePython Route = "python"
)

// String 返回 Route 的稳定字面量。
func (r Route) String() string { return string(r) }

// Valid 报告 Route 是否属于三条固定路由。
func (r Route) Valid() bool {
	switch r {
	case RoutePackages, RouteSimple, RoutePython:
		return true
	default:
		return false
	}
}

// Upstream 描述某条路由按实测顺序排列的上游前缀（每项以 / 结尾的绝对 HTTPS URL）与源 key。
type Upstream struct {
	Key string
	// Base 在 packages 路由是 packages 前缀，simple 路由是 simple 前缀，python 路由是源 base + "/"。
	Base string
	// AcceptRange 由探针得知；未知为 false，HEAD 见到 Accept-Ranges 时中继自行更新。
	AcceptRange bool
	// RewriteFrom 只对 simple 路由有意义：该源自己的 packages 前缀，索引响应体内以它开头的
	// URL 会被改写为 <BaseURL>/packages/。为空时不改写。
	RewriteFrom string
}

// Config 描述一次中继会话的目录、上游与并发参数；零值字段取包内默认值。
type Config struct {
	// StagingDir 是 layout.RelayStagingDir()，中继独占，Close 时清空。
	StagingDir string
	// Upstreams 按路由给出上游列表；至少要有一条非空路由。
	Upstreams map[Route][]Upstream
	// Items 是已知制品，可为空；Register 可追加。
	Items            []Item
	ChunkThreshold   int64
	ChunkSize        int64
	MaxFiles         int
	MaxChunksPerFile int
	StallTimeout     time.Duration
	ConnectTimeout   time.Duration
}

// Progress 是中继的聚合进度：Item/Source 为当前（最近有字节到达的）文件与源；Total 为已登记
// Item 的字节总和，未登记文件在 HEAD 得知大小后并入；Received 为已校验交付的字节 + 进行中
// 文件的已收字节。
type Progress struct {
	Route          Route
	Item           string
	Source         string
	Received       int64
	Total          int64
	BytesPerSecond int64
}

// Deps 是中继的可注入依赖；任一字段为 nil 时取生产默认值（Progress 为不回调）。
type Deps struct {
	// Client 是上游客户端，测试注入。
	Client httpClient
	Clock  func() time.Time
	// Progress 是同步回调；返回错误时中继停止并让后续请求 502。
	Progress func(Progress) error
	Logger   func(level, message string, fields map[string]any)
}

// Outcome 是单次上游尝试的稳定结局分类。
type Outcome string

const (
	// OutcomeConnectTimeout 表示响应头返回前超时。
	OutcomeConnectTimeout Outcome = "connect_timeout"
	// OutcomeReadTimeout 表示响应体读空闲失速。
	OutcomeReadTimeout Outcome = "read_timeout"
	// OutcomeHTTPStatus 表示上游返回了非预期状态码。
	OutcomeHTTPStatus Outcome = "http_status"
	// OutcomeChecksumMismatch 表示取回内容与登记的 sha256 不符。
	OutcomeChecksumMismatch Outcome = "checksum_mismatch"
	// OutcomeNetwork 表示未细分的网络传输故障。
	OutcomeNetwork Outcome = "network"
	// OutcomeCancelled 表示中继关闭或进度回调失败导致取回被取消。
	OutcomeCancelled Outcome = "cancelled"
)

// String 返回 Outcome 的稳定字面量。
func (o Outcome) String() string { return string(o) }

// AttemptOutcome 记录某个源对某个文件的一次尝试结局。
type AttemptOutcome struct {
	Source  string
	Outcome Outcome
}

// FileFailure 记录所有源都失败的文件及其逐源结局。
type FileFailure struct {
	Item     string
	Attempts []AttemptOutcome
}

// Summary 是供 result.details.relay 使用的汇总；Failures 上限 32 项。
type Summary struct {
	Files    int
	Bytes    int64
	BySource map[string]int64
	Failures []FileFailure
}

// httpClient 是上游请求的最小接口，生产默认为带重定向策略的 *http.Client。
type httpClient interface {
	Do(request *http.Request) (*http.Response, error)
}
