package mirror

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestNetworkFailureKind_Structured(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want FailureKind
	}{
		{name: "dns", err: &net.DNSError{Err: "secret", Name: "private.invalid"}, want: "dns_failed"},
		{name: "dns timeout", err: &net.DNSError{IsTimeout: true}, want: "dns_timeout"},
		{name: "connect", err: &net.OpError{Op: "dial", Err: errors.New("secret")}, want: "connect_failed"},
		{name: "connect timeout", err: &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, want: "connect_timeout"},
		{name: "read", err: &net.OpError{Op: "read", Err: errors.New("secret")}, want: "network_failed"},
		{name: "timeout", err: context.DeadlineExceeded, want: "network_timeout"},
		{name: "proxy", err: &net.OpError{Op: "proxyconnect", Err: errors.New("secret")}, want: "proxy_failed"},
		{name: "certificate", err: x509.UnknownAuthorityError{}, want: "tls_certificate"},
		{name: "tls", err: tls.RecordHeaderError{Msg: "secret"}, want: "tls_failed"},
		{name: "unknown", err: errors.New("DNS timeout HTTP 503"), want: FailureNetwork},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NetworkFailureKind(fmt.Errorf("fixture: %w", test.err)); got != test.want {
				t.Fatalf("kind = %s, want %s", got, test.want)
			}
		})
	}
}
