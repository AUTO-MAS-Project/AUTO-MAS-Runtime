package mirror

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// NetworkFailureKind 从结构化错误链提取安全类别；未知错误不根据文本猜测。
func NetworkFailureKind(err error) FailureKind {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		return "proxy_failed"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return "dns_timeout"
		}
		return "dns_failed"
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	if errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &roots) {
		return "tls_certificate"
	}
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &record) || errors.As(err, &alert) {
		return "tls_failed"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		if op != nil && op.Op == "dial" {
			return "connect_timeout"
		}
		return "network_timeout"
	}
	if op != nil {
		if op.Op == "dial" {
			return "connect_failed"
		}
		return "network_failed"
	}
	return FailureNetwork
}
