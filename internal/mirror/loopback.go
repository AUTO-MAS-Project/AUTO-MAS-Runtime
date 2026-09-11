package mirror

import (
	"fmt"
	"net/url"
	"strings"
)

// NewLoopbackRewrite 构造指向回环中继的锁改写前缀（增补 2 C17 第 2 条）。
//
// 只接受 http://127.0.0.1:<port>：这是「规则 8 回环例外」的唯一形态，其它主机或协议一律拒绝，
// 目录里的镜像前缀仍经 NewPackageIndexSource 走 HTTPS 校验。
func NewLoopbackRewrite(baseURL string) (PackageIndexRewrite, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return PackageIndexRewrite{}, fmt.Errorf("%w: loopback base %q", errInvalidSource, baseURL)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return PackageIndexRewrite{}, fmt.Errorf("%w: loopback base %q is not http://127.0.0.1:<port>", errInvalidSource, baseURL)
	}
	base := "http://127.0.0.1:" + parsed.Port()
	return PackageIndexRewrite{
		simpleBase:   base + "/simple",
		packagesBase: base + "/packages/",
	}, nil
}

// IsLoopbackBase 报告地址是否是回环中继地址；用于把回环首项与 HTTPS 上游区分开。
func IsLoopbackBase(value string) bool {
	return strings.HasPrefix(value, "http://127.0.0.1:")
}
