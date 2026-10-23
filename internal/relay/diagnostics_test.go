package relay

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRelay_FailureDiagnostics(t *testing.T) {
	for _, route := range []string{"single", "chunks", "index"} {
		t.Run(route, func(t *testing.T) {
			a, b := newFakeMirror(t, true), newFakeMirror(t, true)
			content := testContent(4096, 1)
			path := wheelPath
			if route == "index" {
				path = simplePath
			}
			a.put(path, content)
			b.put(path, content)
			a.behave(path, mirrorBehavior{status: 401})
			b.behave(path, mirrorBehavior{status: 503})
			cfg := packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, a, b)
			if route == "chunks" {
				cfg.ChunkThreshold = 1024
				cfg.ChunkSize = 1024
			}
			if route == "index" {
				cfg = simpleConfig(a, b)
			}
			fixture := startFixture(t, fixtureOptions{cfg: cfg}, a, b)
			if route != "chunks" {
				fixture.clock.setStep(125 * time.Millisecond)
			}
			response, _ := fixture.get(t, "/"+path)
			if response.StatusCode != 502 {
				t.Fatalf("status = %d, want 502", response.StatusCode)
			}
			failures := fixture.server.Summary().Failures
			if len(failures) != 1 || len(failures[0].Attempts) != 2 {
				t.Fatalf("failures = %#v, want two source failures", failures)
			}
			for _, attempt := range failures[0].Attempts {
				wantStatus := 401
				if attempt.Source == "b" {
					wantStatus = 503
				}
				if attempt.FailureKind != "http_status" || attempt.HTTPStatus != wantStatus || attempt.Outcome != OutcomeHTTPStatus || attempt.Duration < 0 {
					t.Errorf("attempt = %#v, want HTTP %d", attempt, wantStatus)
				}
				if route != "chunks" && attempt.Duration != 125*time.Millisecond {
					t.Errorf("duration = %s, want 125ms", attempt.Duration)
				}
			}
		})
	}
}

type diagnosticClient struct{}

func (diagnosticClient) Do(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, ContentLength: 1, Request: request, Body: io.NopCloser(strings.NewReader("x"))}, nil
}

type diagnosticWriter struct{}

func (diagnosticWriter) Write([]byte) (int, error) { return 0, syscall.Errno(5) }

func TestRelay_LocalFailureDiagnostics(t *testing.T) {
	e := &engine{cfg: Config{ConnectTimeout: time.Second, StallTimeout: time.Second}, client: diagnosticClient{}, timers: newRuntimeTimer}
	t.Run("open", func(t *testing.T) {
		attempt := AttemptOutcome{}
		_, outcome := e.singleStream(t.Context(), fileSpec{}, Upstream{Base: "https://private.invalid/"}, filepath.Join(t.TempDir(), "missing", "archive.part"), map[string]int64{}, &attempt)
		if outcome != OutcomeNetwork || attempt.FailureKind != "filesystem_open" || attempt.Operation != "open" {
			t.Fatalf("outcome=%s attempt=%#v, want filesystem_open", outcome, attempt)
		}
	})
	t.Run("write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://private.invalid/", nil)
		response, _ := (diagnosticClient{}).Do(request)
		handle := &upstreamHandle{response: response, cancel: cancel}
		attempt := AttemptOutcome{}
		_, outcome := e.readBody(t.Context(), handle, diagnosticWriter{}, 1, func(int64) {}, &attempt)
		if outcome != OutcomeNetwork || attempt.FailureKind != "filesystem_write" || attempt.WindowsError != 5 {
			t.Fatalf("outcome=%s attempt=%#v, want filesystem_write and errno 5", outcome, attempt)
		}
	})
	t.Run("publish", func(t *testing.T) {
		attempt := AttemptOutcome{}
		_, ok := e.publish(fileSpec{}, filepath.Join(t.TempDir(), "missing.part"), filepath.Join(t.TempDir(), "output"), nil, &attempt)
		if ok || attempt.FailureKind != "filesystem_publish" || attempt.Operation != "publish" {
			t.Fatalf("attempt=%#v, want filesystem_publish", attempt)
		}
	})
	t.Run("chunk preparation", func(t *testing.T) {
		e.cfg.ChunkThreshold = 1
		e.upstreams = map[Route][]Upstream{RoutePackages: {{Key: "a", AcceptRange: true}, {Key: "b", AcceptRange: true}}}
		plan := newChunkPlan(nil)
		ok := e.tryChunked(t.Context(), fileSpec{key: fileKey{route: RoutePackages}, size: 4}, filepath.Join(t.TempDir(), "missing", "file.part"), plan)
		if ok || len(plan.attempts) != 1 || plan.attempts[0].FailureKind != "filesystem_open" || plan.attempts[0].Source != "" {
			t.Fatalf("attempts=%#v, want unassigned local open failure", plan.attempts)
		}
	})
	t.Run("verification read", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "verify")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		attempt := AttemptOutcome{}
		if verifyDigest(file, []byte{1}, &attempt) || attempt.FailureKind != "filesystem_read" {
			t.Fatalf("attempt=%#v, want filesystem_read", attempt)
		}
	})
}

func TestRelay_DiagnosticCapacityPreservesTransferFailures(t *testing.T) {
	ledger := newLedger()
	for range maxFailures + 1 {
		ledger.failed("index", nil, true)
	}
	ledger.failed("archive.whl", []AttemptOutcome{{Source: "pypi", Outcome: OutcomeNetwork}})
	summary := ledger.summary()
	if len(summary.Failures) != maxFailures || summary.Failures[0].Item != "archive.whl" || summary.Failures[0].DiagnosticOnly {
		t.Fatalf("summary=%#v, want transfer failure retained before bounded index diagnostics", summary)
	}
}
