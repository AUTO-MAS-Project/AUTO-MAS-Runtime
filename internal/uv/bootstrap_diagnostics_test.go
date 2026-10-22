package uv

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestBootstrap_DownloadDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  *mirror.DownloadFailure
		kind   string
		status int
	}{
		{name: "http", cause: &mirror.DownloadFailure{Kind: mirror.FailureHTTPStatus, StatusCode: 503}, kind: "http_status", status: 503},
		{name: "dns", cause: &mirror.DownloadFailure{Kind: mirror.FailureNetwork, Err: &net.DNSError{Err: "secret", Name: "private.invalid"}}, kind: "dns_failed"},
		{name: "certificate", cause: &mirror.DownloadFailure{Kind: mirror.FailureNetwork, Err: x509.UnknownAuthorityError{}}, kind: "tls_certificate"},
		{name: "read timeout", cause: &mirror.DownloadFailure{Kind: mirror.FailureReadTimeout}, kind: "read_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { now = now.Add(125 * time.Millisecond); return now }
			rotator, err := mirror.NewRotator(mirror.WithRotatorClock(clock))
			if err != nil {
				t.Fatal(err)
			}
			artifact := testArtifact("fixture")
			b := newTestBootstrapper(t, newUVTestLayout(t), artifact, &fakeDownloader{err: test.cause}, &fakeVersionChecker{}, nil, nil)
			b.rotator = rotator
			_, err = b.download(t.Context(), testMirrorPolicy(t), artifact, nil)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code() != protocol.CodeUVDownloadFailed {
				t.Fatalf("error = %v, want UV_DOWNLOAD_FAILED", err)
			}
			attempts, ok := failure.Details()["attempts"].([]map[string]any)
			if !ok || len(attempts) == 0 {
				t.Fatalf("details = %#v, want source attempts", failure.Details())
			}
			for i, attempt := range attempts {
				if attempt["failureKind"] != test.kind || attempt["durationMs"] != int64(125) || attempt["globalTry"] != i+1 || attempt["outcome"] != "switch_source" {
					t.Errorf("attempt = %#v, want %s at 125 ms", attempt, test.kind)
				}
				if test.status != 0 && attempt["httpStatus"] != test.status {
					t.Errorf("httpStatus = %v, want %d", attempt["httpStatus"], test.status)
				}
				if test.status == 0 {
					if _, exists := attempt["httpStatus"]; exists {
						t.Errorf("attempt = %#v, want no HTTP status", attempt)
					}
				}
			}
			clean := telemetry.SanitizeDiagnostics(failure.Details())
			encoded, err := json.Marshal(clean)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), test.kind) || !strings.Contains(string(encoded), "durationMs") {
				t.Fatalf("telemetry = %s, want diagnostic facts", encoded)
			}
			for _, secret := range []string{"private.invalid", "secret", "https://"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("telemetry contains %q: %s", secret, encoded)
				}
			}
		})
	}
}
