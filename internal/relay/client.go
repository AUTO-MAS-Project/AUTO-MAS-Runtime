package relay

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxRedirects = 10

var (
	errURLPolicy         = errors.New("upstream url violates https policy")
	errRedirectDowngrade = errors.New("upstream redirect downgrades to http")
	errRedirectLimit     = errors.New("upstream redirect limit exceeded")
)

// timer 是可注入的一次性定时器，形态沿用 mirror 下载器，便于测试推进失速判定。
type timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(delay time.Duration) bool
}

type timerFactory func(delay time.Duration) timer

type runtimeTimer struct {
	timer *time.Timer
}

func (t *runtimeTimer) C() <-chan time.Time { return t.timer.C }

func (t *runtimeTimer) Stop() bool { return t.timer.Stop() }

func (t *runtimeTimer) Reset(delay time.Duration) bool { return t.timer.Reset(delay) }

func newRuntimeTimer(delay time.Duration) timer {
	return &runtimeTimer{timer: time.NewTimer(delay)}
}

// stopAndDrainTimer 停止定时器并吞掉已经触发但尚未被读取的信号，避免下一轮 select 误判失速。
func stopAndDrainTimer(value timer) {
	if value.Stop() {
		return
	}
	select {
	case <-value.C():
	default:
	}
}

// newDefaultHTTPClient 复制默认传输并关闭透明压缩：中继要按字节校验哈希与 Content-Length。
func newDefaultHTTPClient() httpClient {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Go 标准库当前保证默认值为 *http.Transport；保留失败关闭的无网络 client。
		return failingClient{err: errors.New("default http transport is unavailable")}
	}
	return newHTTPClientWithTransport(base.Clone())
}

func newHTTPClientWithTransport(transport *http.Transport) *http.Client {
	configured := transport.Clone()
	configured.DisableCompression = true
	return &http.Client{
		Transport:     configured,
		CheckRedirect: checkRedirect,
	}
}

type failingClient struct {
	err error
}

func (c failingClient) Do(*http.Request) (*http.Response, error) {
	return nil, c.err
}

// checkRedirect 沿用 mirror 下载器策略：最多 10 跳，且每一跳都必须仍是 HTTPS。
func checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errRedirectLimit
	}
	if request == nil || request.URL == nil {
		return errURLPolicy
	}
	if _, err := validateHTTPSURL(request.URL.String()); err != nil {
		if strings.EqualFold(request.URL.Scheme, "http") {
			return errRedirectDowngrade
		}
		return err
	}
	return nil
}

func validateHTTPSURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() ||
		!strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errURLPolicy
	}
	return parsed, nil
}
