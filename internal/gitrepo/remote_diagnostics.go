package gitrepo

import (
	"errors"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// classifyRemoteFailure 只读取结构化错误，不把错误文本、地址或响应正文放入诊断。
func classifyRemoteFailure(err error) (mirror.FailureKind, int) {
	// go-git 的这两个包装没有 Unwrap；显式读取 Err，最多展开 8 层。
	for range 8 {
		var unexpected *plumbing.UnexpectedError
		var permanent *plumbing.PermanentError
		switch {
		case errors.As(err, &unexpected):
			err = unexpected.Err
		case errors.As(err, &permanent):
			err = permanent.Err
		default:
			return classifyRemoteCause(err)
		}
	}
	return failureResolve, 0
}

func classifyRemoteCause(err error) (mirror.FailureKind, int) {
	kind := mirror.NetworkFailureKind(err)
	switch kind {
	case "proxy_failed", "dns_timeout", "dns_failed", "tls_certificate", "tls_failed":
		return kind, 0
	}
	// go-git 对 401/403/404 只保留固定 sentinel，其余状态保留在 http.Err。
	for _, test := range []struct {
		cause  error
		status int
	}{
		{cause: transport.ErrAuthenticationRequired, status: http.StatusUnauthorized},
		{cause: transport.ErrAuthorizationFailed, status: http.StatusForbidden},
		{cause: transport.ErrRepositoryNotFound, status: http.StatusNotFound},
	} {
		if errors.Is(err, test.cause) {
			return "http_status", test.status
		}
	}
	var response *githttp.Err
	if errors.As(err, &response) && response.Response != nil &&
		response.Response.StatusCode >= 100 && response.Response.StatusCode <= 599 {
		return "http_status", response.Response.StatusCode
	}
	if kind != mirror.FailureNetwork {
		return kind, 0
	}
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return "remote_empty", 0
	}
	return failureResolve, 0
}

func fetchNetworkDiagnostics(err error, operation string, fallback mirror.FailureKind) (mirror.FailureKind, map[string]any) {
	kind, status := classifyRemoteFailure(err)
	if errors.Is(err, errGitReferenceBudgetExceeded) || errors.Is(err, errGitCloneBudgetExceeded) {
		kind = "network_timeout"
	} else if kind == failureResolve {
		kind = fallback
	}
	details := map[string]any{"operation": operation}
	if status != 0 {
		details["httpStatus"] = status
	}
	return kind, details
}
