package relay

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
)

// maxSimpleBody 限制索引页体积：真实索引页最多几 MB，超出即视为上游异常。
const maxSimpleBody = 64 << 20

var errSimpleBodyTooLarge = errors.New("simple index body exceeds limit")

// serveSimple 按上游顺序取第一个 200 的索引页，把该源自己的 packages 前缀改写为中继的
// packages 前缀后原样透传：Accept 与 Content-Type 都透传，Content-Length 按改写后的长度重算。
//
// 相对链接（"../../packages/..." 一类）不需要改写：中继的 /simple/<name>/ 与 /packages/
// 保持与 PyPI 相同的相对布局，客户端自行解析即落到中继自己的 /packages/。
func (e *engine) serveSimple(
	ctx context.Context,
	writer http.ResponseWriter,
	request *http.Request,
	name string,
) {
	headers := map[string]string{}
	if accept := request.Header.Get("Accept"); accept != "" {
		headers["Accept"] = accept
	}
	relayPrefix := []byte(e.baseURL + "/" + RoutePackages.String() + "/")
	for _, upstream := range e.snapshotUpstreams(RouteSimple) {
		body, contentType, outcome := e.fetchIndex(ctx, upstream, name, headers)
		if outcome == OutcomeCancelled {
			return
		}
		if outcome != "" {
			e.log("warning", "relay simple index attempt failed", map[string]any{
				"route": RouteSimple.String(), "item": name,
				"source": upstream.Key, "outcome": outcome,
			})
			continue
		}
		if upstream.RewriteFrom != "" {
			body = bytes.ReplaceAll(body, []byte(upstream.RewriteFrom), relayPrefix)
		}
		if contentType != "" {
			writer.Header().Set("Content-Type", contentType)
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.WriteHeader(http.StatusOK)
		// 对端断开时写失败没有可恢复的动作。
		_, _ = writer.Write(body)
		return
	}
	if ctx.Err() != nil {
		return
	}
	writeStatus(writer, http.StatusBadGateway, "relay: all index sources failed")
}

// fetchIndex 取一个上游的索引页；非 200 与超限都算该源失败。
func (e *engine) fetchIndex(
	ctx context.Context,
	upstream Upstream,
	name string,
	headers map[string]string,
) ([]byte, string, Outcome) {
	handle, outcome := e.open(ctx, http.MethodGet, upstream.Base+name, headers)
	if outcome != "" {
		return nil, "", outcome
	}
	defer handle.close()
	response := handle.response
	if response.StatusCode != http.StatusOK {
		return nil, "", OutcomeHTTPStatus
	}
	if response.ContentLength > maxSimpleBody {
		return nil, "", OutcomeNetwork
	}
	var buffer bytes.Buffer
	_, outcome = e.readBody(ctx, handle, &limitedWriter{writer: &buffer, remaining: maxSimpleBody}, 0, func(int64) {})
	if outcome != "" {
		return nil, "", outcome
	}
	return buffer.Bytes(), response.Header.Get("Content-Type"), ""
}

// limitedWriter 在超过 remaining 字节时报错，让读循环把超限当作网络故障处理。
type limitedWriter struct {
	writer    *bytes.Buffer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errSimpleBodyTooLarge
	}
	w.remaining -= int64(len(p))
	return w.writer.Write(p)
}
