package health

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestHealth_TimeoutDiagnostics(t *testing.T) {
	for _, test := range []struct{ name, kind, state string }{
		{name: "connection", kind: "connect_failed"},
		{name: "request timeout", kind: "request_timeout"},
		{name: "starting", kind: "backend_not_ready", state: "starting"},
		{name: "ready flag false", kind: "backend_not_ready", state: "ready"},
		{name: "one success", kind: "readiness_pending", state: "ready"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock()
			result := transportResult{err: &net.OpError{Op: "dial", Err: errors.New("private.invalid password=secret")}}
			if test.state != "" {
				body := healthBody(test.state, "", 1, "v5.4.0", testCommit)
				if test.name == "ready flag false" {
					body = strings.Replace(body, `"ready":true`, `"ready":false`, 1)
				}
				result = transportResult{response: jsonResponse(body)}
			}
			var transport Transport = &sequenceTransport{responses: []transportResult{result}}
			if test.name == "request timeout" {
				transport = &blockingTransport{started: make(chan struct{})}
			}
			checker := NewChecker(Config{Transport: transport, Clock: clock, TotalTimeout: 35 * time.Millisecond, RequestTimeout: 5 * time.Millisecond, PollInterval: 7 * time.Millisecond})
			done := make(chan error, 1)
			go func() { done <- checker.Check(t.Context(), managedExpectation(), testProbe()) }()
			if test.name == "request timeout" {
				clock.WaitForTimer(t, 5*time.Millisecond)
				clock.Fire(5 * time.Millisecond)
			}
			clock.WaitForTimer(t, 7*time.Millisecond)
			clock.Fire(35 * time.Millisecond)
			err := <-done
			var failure *Error
			if !errors.As(err, &failure) || failure.Code() != protocol.CodeBackendHealthTimeout {
				t.Fatalf("error = %v, want BACKEND_HEALTH_TIMEOUT", err)
			}
			details := failure.Details()
			if details["failureKind"] != test.kind || details["attemptCount"] != 1 || details["port"] != DefaultPort || details["durationMs"] != int64(0) {
				t.Fatalf("details = %#v, want %s with one request", details, test.kind)
			}
			if test.state != "" && details["state"] != test.state {
				t.Errorf("state = %v, want %s", details["state"], test.state)
			}
			clean := telemetry.SanitizeDiagnostics(details)
			if clean["failureKind"] != test.kind || clean["attemptCount"] != 1 {
				t.Fatalf("telemetry = %#v, want timeout facts", clean)
			}
			encoded, _ := json.Marshal(clean)
			for _, secret := range []string{"private.invalid", "password", "secret", "http://"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("diagnostics = %s, contains %q", encoded, secret)
				}
			}
		})
	}
}
