package gitrepo

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestFetcher_NetworkDiagnostics(t *testing.T) {
	secret := errors.New("https://user:password@private.invalid/repo?token=secret-response")
	for _, operation := range []string{"reference_discovery", "clone"} {
		budgetErr := errGitReferenceBudgetExceeded
		if operation == "clone" {
			budgetErr = errGitCloneBudgetExceeded
		}
		for _, test := range []struct {
			name   string
			cause  error
			kind   string
			status int
		}{
			{name: "dns", cause: &net.DNSError{Err: secret.Error(), Name: "private.invalid"}, kind: "dns_failed"},
			{name: "tls", cause: x509.UnknownAuthorityError{}, kind: "tls_certificate"},
			{name: "http", cause: &githttp.Err{Response: &http.Response{StatusCode: 503}}, kind: "http_status", status: 503},
			{name: "transport budget", cause: budgetErr, kind: "network_timeout"},
			{name: "unknown", cause: secret},
			{name: "text is not parsed", cause: errors.New("proxyconnect dns timeout x509 HTTP 407")},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				target := mustParseTarget(t, "v5.4.0")
				var cause error = plumbing.NewPermanentError(plumbing.NewUnexpectedError(test.cause))
				if test.name == "transport budget" {
					// 内部预算由 Fetcher 自己替换为 sentinel，不能沿错误链混入用户取消。
					cause = test.cause
				}
				client := &fakeGitClient{
					list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
						if operation == "reference_discovery" {
							return nil, cause
						}
						return targetBranchReferences(target), nil
					},
					clone: func(context.Context, string, git.CloneOptions) error { return cause },
				}
				deps := successfulFetcherDependencies(t, client)
				now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
				rotator, err := mirror.NewRotator(mirror.WithMaxSourceAttempts(1), mirror.WithRotatorClock(func() time.Time {
					now = now.Add(125 * time.Millisecond)
					return now
				}))
				if err != nil {
					t.Fatal(err)
				}
				deps.rotator = rotator
				_, err = mustTestFetcher(t, deps).Fetch(t.Context(), FetchRequest{
					Plan: mustGitPlan(t, "cnb"), Target: target, OperationID: "DIAGNOSTIC-1",
				})
				wantCode, kind := protocol.CodeGitRemoteResolveFailed, test.kind
				if operation == "clone" {
					wantCode = protocol.CodeGitCloneFailed
				}
				if kind == "" {
					kind = "resolve_failed"
					if operation == "clone" {
						kind = "clone_failed"
					}
				}
				assertGitrepoCode(t, err, wantCode)
				var failure *Error
				if !errors.As(err, &failure) || failure.Stage() != protocol.StageWorkspaceClone {
					t.Fatalf("error = %v, want workspace.clone failure", err)
				}
				attempts := failure.Details()["attempts"].([]map[string]any)
				if len(attempts) != 2 {
					t.Fatalf("attempts = %#v, want two sources", attempts)
				}
				for i, attempt := range attempts {
					if attempt["failureKind"] != kind || attempt["operation"] != operation || attempt["durationMs"] != int64(125) ||
						attempt["sourceTry"] != 1 || attempt["globalTry"] != i+1 || attempt["outcome"] != "retry_same_source" {
						t.Errorf("attempt = %#v, want %s/%s at 125 ms", attempt, operation, kind)
					}
					if test.status != 0 && attempt["httpStatus"] != test.status {
						t.Errorf("attempt = %#v, want HTTP %d", attempt, test.status)
					}
					if test.status == 0 {
						if _, exists := attempt["httpStatus"]; exists {
							t.Errorf("attempt = %#v, want no HTTP status", attempt)
						}
					}
				}
				for _, details := range []map[string]any{failure.Details(), telemetry.SanitizeDiagnostics(failure.Details())} {
					encoded, err := json.Marshal(details)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(encoded), kind) || !strings.Contains(string(encoded), operation) {
						t.Errorf("details = %s, want %s/%s", encoded, kind, operation)
					}
					for _, secret := range []string{"private.invalid", "secret-response", "password", "https://"} {
						if strings.Contains(string(encoded), secret) {
							t.Errorf("details = %s, want no %q", encoded, secret)
						}
					}
				}
			})
		}
	}
}

func TestFetcher_MixedDiagnosticAttempts(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	calls := 0
	client := &fakeGitClient{
		list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
			calls++
			if calls == 1 {
				return nil, &githttp.Err{Response: &http.Response{StatusCode: 503}}
			}
			return targetBranchReferences(target), nil
		},
		clone: func(context.Context, string, git.CloneOptions) error { return &net.DNSError{Err: "fixture"} },
	}
	_, err := mustTestFetcher(t, successfulFetcherDependencies(t, client)).Fetch(t.Context(), FetchRequest{
		Plan: mustGitPlan(t, "cnb"), Target: target, OperationID: "DIAGNOSTIC-2",
	})
	assertGitrepoCode(t, err, protocol.CodeGitCloneFailed)
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	attempts := failure.Details()["attempts"].([]map[string]any)
	if len(attempts) != 2 || attempts[0]["operation"] != "reference_discovery" || attempts[0]["httpStatus"] != 503 ||
		attempts[1]["operation"] != "clone" || attempts[1]["failureKind"] != "dns_failed" {
		t.Fatalf("attempts = %#v, want independent reference/clone facts", attempts)
	}
	if _, exists := attempts[1]["httpStatus"]; exists {
		t.Errorf("attempt = %#v, want no previous HTTP status", attempts[1])
	}
}

func TestFetcher_CleanupOverridesNetworkDiagnostics(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	deps := successfulFetcherDependencies(t, &fakeGitClient{
		list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
			return targetBranchReferences(target), nil
		},
		clone: func(context.Context, string, git.CloneOptions) error {
			return &githttp.Err{Response: &http.Response{StatusCode: 503}}
		},
	})
	deps.remover = &fakeTreeRemover{remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
		return filesystem.DeleteResult{}, errors.New("fixture cleanup failure")
	}}
	_, err := mustTestFetcher(t, deps).Fetch(t.Context(), FetchRequest{
		Plan: mustGitPlan(t, "cnb"), Target: target, OperationID: "DIAGNOSTIC-3",
	})
	assertGitrepoCode(t, err, protocol.CodeGitRepoCleanupFailed)
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	for _, attempt := range failure.Details()["attempts"].([]map[string]any) {
		if attempt["failureKind"] != "cleanup_failed" {
			t.Errorf("attempt = %#v, want cleanup_failed", attempt)
		}
		if _, exists := attempt["httpStatus"]; exists {
			t.Errorf("attempt = %#v, want no overridden HTTP status", attempt)
		}
	}
}
