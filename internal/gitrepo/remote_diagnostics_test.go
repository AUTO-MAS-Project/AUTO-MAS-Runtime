package gitrepo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestRemoteResolve_FailureDiagnostics(t *testing.T) {
	secret := errors.New("https://user:password@private-host.invalid/repo?token=secret-response")
	for _, test := range []struct {
		name   string
		err    error
		kind   string
		status int
	}{
		{name: "dns", err: &net.DNSError{Err: secret.Error(), Name: "private-host.invalid"}, kind: "dns_failed"},
		{name: "dns timeout", err: &net.DNSError{Err: secret.Error(), IsTimeout: true}, kind: "dns_timeout"},
		{name: "connect", err: &net.OpError{Op: "dial", Net: "tcp", Err: secret}, kind: "connect_failed"},
		{name: "connect timeout", err: &net.OpError{Op: "dial", Net: "tcp", Err: remoteTimeoutError{}}, kind: "connect_timeout"},
		{name: "read timeout", err: &net.OpError{Op: "read", Net: "tcp", Err: remoteTimeoutError{}}, kind: "network_timeout"},
		{name: "network", err: &net.OpError{Op: "read", Net: "tcp", Err: secret}, kind: "network_failed"},
		{name: "proxy", err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: secret}, kind: "proxy_failed"},
		{name: "unknown authority", err: x509.UnknownAuthorityError{}, kind: "tls_certificate"},
		{name: "hostname", err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "private-host.invalid"}, kind: "tls_certificate"},
		{name: "expired", err: x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, kind: "tls_certificate"},
		{name: "certificate wrapper", err: &tls.CertificateVerificationError{Err: secret}, kind: "tls_certificate"},
		{name: "tls protocol", err: tls.RecordHeaderError{Msg: secret.Error()}, kind: "tls_failed"},
		{name: "tls alert", err: tls.AlertError(40), kind: "tls_failed"},
		{name: "unauthorized", err: fmt.Errorf("%w: %v", transport.ErrAuthenticationRequired, secret), kind: "http_status", status: 401},
		{name: "forbidden", err: fmt.Errorf("%w: %v", transport.ErrAuthorizationFailed, secret), kind: "http_status", status: 403},
		{name: "not found", err: fmt.Errorf("%w: %v", transport.ErrRepositoryNotFound, secret), kind: "http_status", status: 404},
		{name: "http", err: plumbing.NewUnexpectedError(&githttp.Err{Response: &http.Response{StatusCode: 503}}), kind: "http_status", status: 503},
		{name: "empty remote", err: transport.ErrEmptyRemoteRepository, kind: "remote_empty"},
		{name: "unknown", err: secret, kind: "resolve_failed"},
		{name: "natural language is not parsed", err: errors.New("proxyconnect dns timeout x509 HTTP 407"), kind: "resolve_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// go-git 的 UnexpectedError 没有 Unwrap，实际网络错误会藏在其 Err 中。
			cause := fmt.Errorf("fixture: %w", plumbing.NewUnexpectedError(&url.Error{Op: "Get", URL: secret.Error(), Err: test.err}))
			client := &fakeGitClient{list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
				return nil, cause
			}}
			_, err := resolveRemoteWithClient(t.Context(), mustGitPlan(t, "cnb"), mustParseTarget(t, "v1.0.0"), client)
			checkRemoteDiagnostics(t, err, test.kind, test.status)
		})
	}
}

func TestRemoteResolve_CheckStage(t *testing.T) {
	for _, name := range []string{"missing branch", "offline", "cancelled", "deadline"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			plan := mustGitPlan(t, "cnb")
			wantCode := protocol.CodeGitBranchNotFound
			calls := 0
			if name == "offline" {
				catalog, err := mirror.DefaultCatalog()
				if err != nil {
					t.Fatal(err)
				}
				policy, err := mirror.NewPolicy(mirror.PolicySpec{Offline: true})
				if err != nil {
					t.Fatal(err)
				}
				plan, err = mirror.BuildPlan(catalog, policy, mirror.KindGit)
				if err != nil {
					t.Fatal(err)
				}
				wantCode = protocol.CodeNetworkUnavailable
			}
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantCode = protocol.CodeOperationCancelled
			}
			if name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Time{})
				defer cancel()
				wantCode = protocol.CodeOperationCancelled
			}
			client := &fakeGitClient{list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
				calls++
				return nil, nil
			}}
			_, err := resolveRemoteWithClient(ctx, plan, mustParseTarget(t, "v1.0.0"), client)
			var operationErr *Error
			if !errors.As(err, &operationErr) || operationErr.Code() != wantCode || operationErr.Stage() != protocol.StageWorkspaceCheck {
				t.Fatalf("resolve error = %#v, want %s at workspace.check", err, wantCode)
			}
			if name != "missing branch" && calls != 0 {
				t.Fatalf("network calls = %d, want 0", calls)
			}
		})
	}
}

func TestRemoteResolve_AttemptDurations(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	calls := 0
	client := &fakeGitClient{list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
		calls++
		now = now.Add(time.Duration(calls) * 125 * time.Millisecond)
		return nil, &net.DNSError{Err: "fixture", Name: "private-host.invalid"}
	}}
	_, err := resolveRemoteWithClient(t.Context(), mustGitPlan(t, "cnb"), mustParseTarget(t, "v1.0.0"), client,
		mirror.WithRotatorClock(func() time.Time { return now }))
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("resolve error = %v, want typed failure", err)
	}
	attempts := operationErr.Details()["attempts"].([]map[string]any)
	if len(attempts) != 2 || attempts[0]["durationMs"] != int64(125) || attempts[1]["durationMs"] != int64(250) {
		t.Fatalf("attempts = %#v, want durations 125 and 250 ms", attempts)
	}
}

func TestRemoteResolve_MixedFailures(t *testing.T) {
	for _, resolveFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(resolveFirst), func(t *testing.T) {
			calls := 0
			client := &fakeGitClient{list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
				calls++
				if (calls == 1) == resolveFirst {
					return nil, plumbing.NewPermanentError(plumbing.NewUnexpectedError(
						&githttp.Err{Response: &http.Response{StatusCode: 503}}))
				}
				return nil, nil
			}}
			_, err := resolveRemoteWithClient(t.Context(), mustGitPlan(t, "cnb"), mustParseTarget(t, "v1.0.0"), client)
			var operationErr *Error
			if !errors.As(err, &operationErr) || operationErr.Code() != protocol.CodeGitRemoteResolveFailed ||
				operationErr.Stage() != protocol.StageWorkspaceCheck || calls != 2 {
				t.Fatalf("resolve = %v, calls %d, want remote resolve failure at workspace.check after two attempts", err, calls)
			}
			attempts := operationErr.Details()["attempts"].([]map[string]any)
			for i, attempt := range attempts {
				want := "branch_not_found"
				if (i == 0) == resolveFirst {
					want = "http_status"
					if attempt["httpStatus"] != 503 {
						t.Errorf("attempt = %#v, want HTTP 503", attempt)
					}
				} else if _, present := attempt["httpStatus"]; present {
					t.Errorf("attempt = %#v, want no HTTP status for missing branch", attempt)
				}
				if attempt["failureKind"] != want {
					t.Errorf("failureKind = %v, want %s", attempt["failureKind"], want)
				}
			}
		})
	}
}

func TestRemoteResolve_HTTPComponentDiagnostics(t *testing.T) {
	for _, status := range []int{401, 403, 404, 407, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "secret-response user:password@private-host.invalid token=private-token")
			}))
			t.Cleanup(server.Close)
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			client := &fakeGitClient{list: func(ctx context.Context, _ string, _ []byte) ([]*plumbing.Reference, error) {
				return (goGitClient{}).ListReferences(ctx, server.URL+"/repo.git", ca)
			}}
			_, err := resolveRemoteWithClient(t.Context(), mustGitPlan(t, "cnb"), mustParseTarget(t, "v1.0.0"), client)
			checkRemoteDiagnostics(t, err, "http_status", status)
		})
	}
}

func TestRemoteResolve_TLSDiagnostics(t *testing.T) {
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	client := &fakeGitClient{list: func(ctx context.Context, _ string, _ []byte) ([]*plumbing.Reference, error) {
		return (goGitClient{}).ListReferences(ctx, server.URL+"/repo.git", nil)
	}}
	_, err := resolveRemoteWithClient(t.Context(), mustGitPlan(t, "cnb"), mustParseTarget(t, "v1.0.0"), client)
	checkRemoteDiagnostics(t, err, "tls_certificate", 0)
}

func checkRemoteDiagnostics(t *testing.T, err error, kind string, status int) {
	t.Helper()
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Code() != protocol.CodeGitRemoteResolveFailed {
		t.Fatalf("resolve error = %#v, want GIT_REMOTE_RESOLVE_FAILED", err)
	}
	if operationErr.Stage() != protocol.StageWorkspaceCheck {
		t.Errorf("stage = %s, want workspace.check", operationErr.Stage())
	}
	attempts, ok := operationErr.Details()["attempts"].([]map[string]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("attempts = %#v, want two source attempts", operationErr.Details()["attempts"])
	}
	for i, attempt := range attempts {
		if attempt["failureKind"] != kind || attempt["outcome"] != "switch_source" ||
			attempt["sourceTry"] != 1 || attempt["globalTry"] != i+1 {
			t.Errorf("attempt = %#v, want %s with original rotation fields", attempt, kind)
		}
		if duration, ok := attempt["durationMs"].(int64); !ok || duration < 0 {
			t.Errorf("durationMs = %#v, want nonnegative integer", attempt["durationMs"])
		}
		if status != 0 && attempt["httpStatus"] != status {
			t.Errorf("httpStatus = %#v, want %d", attempt["httpStatus"], status)
		}
		if status == 0 {
			if _, present := attempt["httpStatus"]; present {
				t.Errorf("attempt = %#v, want no unconfirmed HTTP status", attempt)
			}
		}
	}
	clean := telemetry.SanitizeDiagnostics(operationErr.Details())
	cleanAttempts, ok := clean["attempts"].([]any)
	if !ok || len(cleanAttempts) != 2 {
		t.Fatalf("sanitized attempts = %#v, want two", clean["attempts"])
	}
	for i, item := range cleanAttempts {
		attempt := item.(map[string]any)
		if attempt["failureKind"] != kind || attempt["durationMs"] != attempts[i]["durationMs"] ||
			(status != 0 && attempt["httpStatus"] != status) {
			t.Errorf("sanitized attempt = %#v, want complete safe diagnostics", attempt)
		}
	}
	encoded, err := json.Marshal(map[string]any{"message": operationErr.Message(), "details": operationErr.Details(), "telemetry": clean})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "private-host", "secret-response", "private-token", "https://", "127.0.0.1"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("diagnostics contain %q: %s", forbidden, encoded)
		}
	}
}

type remoteTimeoutError struct{}

func (remoteTimeoutError) Error() string   { return "private-host.invalid timed out" }
func (remoteTimeoutError) Timeout() bool   { return true }
func (remoteTimeoutError) Temporary() bool { return true }
