package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	// developmentDefaultPort 是 development 模式的缺省端口（增补 1 C12）：与 AUTO-MAS dev
	// 已合入的「开发版 36164 / 正式版 36163 并存」约定一致，让受监督的开发版不撞正式版。
	developmentDefaultPort = 36164
	backendClosePath       = "/api/core/close"
)

// defaultPortForMode 返回模式的缺省受监督端口：managed 沿用 health.DefaultPort。
func defaultPortForMode(mode Mode) int {
	if mode == ModeDevelopment {
		return developmentDefaultPort
	}
	return health.DefaultPort
}

// resolveSupervisedPort 把 Request.Port 解析成最终端口：零值按模式取缺省，越界失败关闭。
// 这里是 backend 内唯一的解析点，首启、单次自动重启、managed 与 development 都读写回
// Request 的值，因此同一监督进程生命周期内端口不变。
func resolveSupervisedPort(request Request, mode Mode) (int, error) {
	port := request.Port
	if port == 0 {
		port = defaultPortForMode(mode)
	}
	if !health.ValidPort(port) {
		return 0, newError(protocol.CodeInvalidArgument, protocol.StageBackendSpawn, "受监督端口超出范围", map[string]any{
			"field": "port",
			"port":  port,
		}, errors.New("supervised port is out of range"))
	}
	return port, nil
}

// backendCloseURL 返回派生端口下的优雅关闭地址。
func backendCloseURL(port int) string {
	return health.BaseURL(port) + backendClosePath
}

// loopbackHTTPCloser 向派生端口的 /api/core/close 发起关闭请求。
type loopbackHTTPCloser struct {
	port int
}

func newLoopbackHTTPCloser(port int) loopbackHTTPCloser {
	return loopbackHTTPCloser{port: port}
}

func (c loopbackHTTPCloser) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("backend shutdown context is nil")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, backendCloseURL(c.port), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	readErr := error(nil)
	if response.Body != nil {
		_, readErr = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	}
	closeErr := error(nil)
	if response.Body != nil {
		closeErr = response.Body.Close()
	}
	client.CloseIdleConnections()
	statusErr := error(nil)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		statusErr = fmt.Errorf("backend close returned status %d", response.StatusCode)
	}
	return errors.Join(statusErr, readErr, closeErr)
}

var _ HTTPCloser = loopbackHTTPCloser{}
