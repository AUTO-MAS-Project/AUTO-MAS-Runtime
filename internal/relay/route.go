package relay

import (
	"net/http"
	"net/url"
	"strings"
)

// routeStatus 描述路径与方法校验的结论：ok 为 false 时 status 是应回给请求方的状态码。
type routeStatus struct {
	route  Route
	path   string
	status int
}

// resolveRoute 把请求路径规范化到 (路由, 上游相对路径)；路径保持百分号编码原样。
//
// 只接受三个固定前缀；路径任一段为空、"." 或 ".."（编码前后都检查）即视为穿越并回 404，
// 不做任何归一化后再放行的尝试。/simple/ 只接受 GET，另两条接受 GET / HEAD。
func resolveRoute(request *http.Request) routeStatus {
	rawPath := request.URL.EscapedPath()
	route, rest, ok := splitRoutePrefix(rawPath)
	if !ok {
		return routeStatus{status: http.StatusNotFound}
	}
	if !methodAllowed(route, request.Method) {
		return routeStatus{route: route, status: http.StatusMethodNotAllowed}
	}
	if route == RouteSimple {
		name := strings.TrimSuffix(rest, "/")
		if !validSegment(name) || strings.Contains(name, "/") {
			return routeStatus{route: route, status: http.StatusNotFound}
		}
		return routeStatus{route: route, path: name + "/"}
	}
	if rest == "" || strings.HasSuffix(rest, "/") {
		return routeStatus{route: route, status: http.StatusNotFound}
	}
	for _, segment := range strings.Split(rest, "/") {
		if !validSegment(segment) {
			return routeStatus{route: route, status: http.StatusNotFound}
		}
	}
	return routeStatus{route: route, path: rest}
}

func splitRoutePrefix(rawPath string) (Route, string, bool) {
	for _, route := range []Route{RoutePackages, RouteSimple, RoutePython} {
		prefix := "/" + route.String() + "/"
		if strings.HasPrefix(rawPath, prefix) {
			return route, strings.TrimPrefix(rawPath, prefix), true
		}
	}
	return "", "", false
}

func methodAllowed(route Route, method string) bool {
	switch method {
	case http.MethodGet:
		return true
	case http.MethodHead:
		return route != RouteSimple
	default:
		return false
	}
}

// validSegment 同时检查编码前与解码后的段：解码失败也视为无效，避免把畸形编码转发给上游。
func validSegment(segment string) bool {
	if !plainSegment(segment) {
		return false
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return false
	}
	return plainSegment(decoded) && !strings.ContainsAny(decoded, `/\`)
}

func plainSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for _, character := range segment {
		if character <= '\x1f' || character == '\x7f' || character == '\\' {
			return false
		}
	}
	return true
}
