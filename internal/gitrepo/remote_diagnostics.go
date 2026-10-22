package gitrepo

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
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
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		return "proxy_failed", 0
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return "dns_timeout", 0
		}
		return "dns_failed", 0
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	if errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &roots) {
		return "tls_certificate", 0
	}
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &record) || errors.As(err, &alert) {
		return "tls_failed", 0
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
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		if op != nil && op.Op == "dial" {
			return "connect_timeout", 0
		}
		return "network_timeout", 0
	}
	if op != nil {
		if op.Op == "dial" {
			return "connect_failed", 0
		}
		return "network_failed", 0
	}
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return "remote_empty", 0
	}
	return failureResolve, 0
}
