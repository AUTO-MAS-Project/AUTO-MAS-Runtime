package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestHealth_RequiresAllNineConditions(t *testing.T) {
	fields := []string{"ready", "backgroundStatus", "backgroundError", "protocol", "version", "commit"}
	for _, field := range fields {
		t.Run("missing_"+field, func(t *testing.T) {
			body := healthBody("ready", "", 1, "v5.4.0", testCommit)
			body = removeJSONField(body, field)
			rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(body)}, {response: jsonResponse(body)}}}
			err := testChecker(rt).Check(t.Context(), Expectation{Mode: ModeManaged, Protocol: 1, Version: "v5.4.0", Commit: testCommit}, testProbe())
			want := protocol.CodeBackendHealthInvalid
			if field == "protocol" || field == "version" || field == "commit" {
				want = protocol.CodeBackendIdentityMismatch
			}
			assertHealthCode(t, err, want)
		})
	}
	for name, body := range map[string]string{
		"type_ready":             `{"ready":"true","backgroundStatus":"ready","backgroundError":null,"protocol":1,"version":"v5.4.0","commit":"` + testCommit + `"}`,
		"type_background_status": `{"ready":true,"backgroundStatus":1,"backgroundError":null,"protocol":1,"version":"v5.4.0","commit":"` + testCommit + `"}`,
		"type_background_error":  `{"ready":true,"backgroundStatus":"ready","backgroundError":false,"protocol":1,"version":"v5.4.0","commit":"` + testCommit + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(body)}}}
			assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()), protocol.CodeBackendHealthInvalid)
		})
	}

	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}, {response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
	probe := testProbe()
	if err := testChecker(rt).Check(t.Context(), Expectation{Mode: ModeManaged, Protocol: 1, Version: "v5.4.0", Commit: testCommit}, probe); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if got, want := probe.calls, 2; got != want {
		t.Fatalf("Probe Healthy calls = %d, want %d", got, want)
	}
}

func TestHealth_JSONSchemaRejectsDuplicateAndAllowsUnknownFields(t *testing.T) {
	unknown := `{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":1,"version":"v5.4.0","commit":"` + testCommit + `","future":true}`
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(unknown)}, {response: jsonResponse(unknown)}}}
	if err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()); err != nil {
		t.Fatalf("unknown field Check() error = %v, want nil", err)
	}
	duplicate := `{"ready":true,"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":1,"version":"v5.4.0","commit":"` + testCommit + `"}`
	duplicateTransport := &sequenceTransport{responses: []transportResult{{response: jsonResponse(duplicate)}}}
	assertHealthCode(t, testChecker(duplicateTransport).Check(t.Context(), managedExpectation(), testProbe()), protocol.CodeBackendHealthInvalid)
}

func TestHealth_PollingStartingRunningAndConnectionRefused(t *testing.T) {
	rt := &sequenceTransport{responses: []transportResult{
		{err: errors.New("connection refused")},
		{response: jsonResponse(healthBody("starting", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("running", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
	}}
	if err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if got, want := rt.calls(), 5; got != want {
		t.Fatalf("RoundTrip calls = %d, want %d", got, want)
	}
}

func TestHealth_BackgroundFailureIsImmediate(t *testing.T) {
	for name, body := range map[string]string{
		"failed":           healthBody("failed", "", 1, "v5.4.0", testCommit),
		"background_error": healthBody("starting", "database unavailable", 1, "v5.4.0", testCommit),
	} {
		t.Run(name, func(t *testing.T) {
			rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(body)}}}
			err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe())
			assertHealthCode(t, err, protocol.CodeBackendHealthInvalid)
			if name == "background_error" {
				assertDetailsOmit(t, err, "database unavailable")
			}
			if got := rt.calls(); got != 1 {
				t.Fatalf("RoundTrip calls = %d, want 1", got)
			}
		})
	}
}

func TestHealth_Non200AndUnknownStatusAreImmediate(t *testing.T) {
	tests := map[string]*http.Response{
		"non200":    {StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("temporarily unavailable"))},
		"unknown":   jsonResponse(healthBody("mystery", "", 1, "v5.4.0", testCommit)),
		"cancelled": jsonResponse(healthBody("cancelled", "", 1, "v5.4.0", testCommit)),
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			rt := &sequenceTransport{responses: []transportResult{{response: response}}}
			assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()), protocol.CodeBackendHealthInvalid)
			if got := rt.calls(); got != 1 {
				t.Fatalf("RoundTrip calls = %d, want 1", got)
			}
		})
	}
	t.Run("unknown_precedes_missing_identity", func(t *testing.T) {
		response := jsonResponse(`{"ready":true,"backgroundStatus":"mystery","backgroundError":null}`)
		rt := &sequenceTransport{responses: []transportResult{{response: response}}}
		assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()), protocol.CodeBackendHealthInvalid)
	})
}

func TestHealth_JobProbeFailureIsImmediate(t *testing.T) {
	for name, probe := range map[string]*fakeProbe{
		"unhealthy": func() *fakeProbe {
			p := testProbe()
			p.healthy = false
			return p
		}(),
		"error": func() *fakeProbe {
			p := testProbe()
			p.probeErr = errors.New("job snapshot failed")
			return p
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
			assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), probe), protocol.CodeBackendHealthInvalid)
		})
	}
}

func TestHealth_TransportAndRequestContract(t *testing.T) {
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}, {response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
	if err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if got := rt.lastURL; got != HealthURL {
		t.Fatalf("request URL = %q, want %q", got, HealthURL)
	}
	defaultChecker := NewChecker(Config{})
	transport, ok := defaultChecker.transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport type = %T, want *http.Transport", defaultChecker.transport)
	}
	if transport.Proxy != nil {
		t.Fatal("default transport proxy is configured, want disabled")
	}
}

func TestHealth_ErrorDetailsAreDefensive(t *testing.T) {
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("failed", "", 1, "v5.4.0", testCommit))}}}
	var healthErr *Error
	if err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()); !errors.As(err, &healthErr) {
		t.Fatalf("Check() error = %T %v, want *Error", err, err)
	}
	details := healthErr.Details()
	details["field"] = "mutated"
	if got := healthErr.Details()["field"]; got == "mutated" {
		t.Fatal("Error.Details() returned an aliased map")
	}
	if _, ok := healthErr.Details()["backgroundStatus"]; ok {
		t.Fatal("Error.Details() exposed raw background status")
	}
}

func TestHealth_IdentityMismatch(t *testing.T) {
	tests := map[string]string{
		"version":       healthBody("ready", "", 1, "v5.4.1", testCommit),
		"commit":        healthBody("ready", "", 1, "v5.4.0", strings.Repeat("f", 40)),
		"protocol":      healthBody("ready", "", 2, "v5.4.0", testCommit),
		"type_protocol": `{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":"1","version":"v5.4.0","commit":"` + testCommit + `"}`,
		"type_version":  `{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":1,"version":54,"commit":"` + testCommit + `"}`,
		"type_commit":   `{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":1,"version":"v5.4.0","commit":54}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(body)}}}
			err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe())
			assertHealthCode(t, err, protocol.CodeBackendIdentityMismatch)
			if name == "version" {
				assertDetailsOmit(t, err, "v5.4.1")
			}
		})
	}
}

func TestHealth_ResetsConsecutiveSuccess(t *testing.T) {
	rt := &sequenceTransport{responses: []transportResult{
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("starting", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
	}}
	if err := testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if got, want := rt.calls(), 4; got != want {
		t.Fatalf("RoundTrip calls = %d, want %d", got, want)
	}
}

func TestHealth_ProcessExitBeforeReady(t *testing.T) {
	probe := testProbe()
	close(probe.exited)
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("starting", "", 1, "v5.4.0", testCommit))}}}
	assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), probe), protocol.CodeBackendExitedBeforeReady)
}

func TestHealth_RequestAndTotalTimeout(t *testing.T) {
	clock := newManualClock()
	rt := &blockingTransport{started: make(chan struct{})}
	checker := NewChecker(Config{
		Transport:            rt,
		Clock:                clock,
		TotalTimeout:         35 * time.Millisecond,
		RequestTimeout:       5 * time.Millisecond,
		PollInterval:         10 * time.Millisecond,
		ConsecutiveSuccesses: 2,
	})
	result := make(chan error, 1)
	go func() {
		result <- checker.Check(t.Context(), managedExpectation(), testProbe())
	}()
	<-rt.started
	clock.WaitForTimer(t, 5*time.Millisecond)
	clock.Fire(5 * time.Millisecond)
	clock.Fire(35 * time.Millisecond)
	err := <-result
	assertHealthCode(t, err, protocol.CodeBackendHealthTimeout)

	t.Run("completed_response_cannot_bypass_request_timer", func(t *testing.T) {
		ctx := t.Context()
		total := make(chan time.Time)
		requestTimeout := make(chan time.Time, 1)
		requestTimeout <- time.Time{}
		result := completedRequestResult(ctx, nil, total, requestTimeout, requestResult{kind: requestResponse, status: http.StatusOK})
		if result.kind != requestTimedOut {
			t.Fatalf("completedRequestResult() kind = %d, want %d", result.kind, requestTimedOut)
		}
	})
}

func TestHealth_TotalTimeoutWinsReadyResponse(t *testing.T) {
	clock := newManualClock()
	transport := &barrierTransport{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit)),
	}
	checker := NewChecker(Config{Transport: transport, Clock: clock, TotalTimeout: 40 * time.Millisecond, RequestTimeout: time.Second, PollInterval: time.Millisecond, ConsecutiveSuccesses: 2})
	result := make(chan error, 1)
	go func() { result <- checker.Check(t.Context(), managedExpectation(), testProbe()) }()
	<-transport.started
	clock.Fire(40 * time.Millisecond)
	close(transport.release)
	assertHealthCode(t, <-result, protocol.CodeBackendHealthTimeout)
}

func TestHealth_TotalTimeoutWinsProbeResult(t *testing.T) {
	clock := newManualClock()
	probe := &barrierProbe{started: make(chan struct{}), release: make(chan struct{})}
	body := jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))
	rt := &sequenceTransport{responses: []transportResult{{response: body}}}
	checker := NewChecker(Config{Transport: rt, Clock: clock, TotalTimeout: 40 * time.Millisecond, RequestTimeout: time.Second, PollInterval: time.Millisecond, ConsecutiveSuccesses: 2})
	result := make(chan error, 1)
	go func() { result <- checker.Check(t.Context(), managedExpectation(), probe) }()
	<-probe.started
	clock.Fire(40 * time.Millisecond)
	close(probe.release)
	assertHealthCode(t, <-result, protocol.CodeBackendHealthTimeout)
}

func TestHealth_DevelopmentOnlyChecksProtocol(t *testing.T) {
	body := `{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":1}`
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(body)}, {response: jsonResponse(body)}}}
	if err := testChecker(rt).Check(t.Context(), Expectation{Mode: ModeDevelopment, Protocol: 1, Version: "ignored", Commit: "ignored"}, testProbe()); err != nil {
		t.Fatalf("development Check() error = %v, want nil", err)
	}

	bad := &sequenceTransport{responses: []transportResult{{response: jsonResponse(`{"ready":true,"backgroundStatus":"ready","backgroundError":null,"protocol":2}`)}}}
	assertHealthCode(t, testChecker(bad).Check(t.Context(), Expectation{Mode: ModeDevelopment, Protocol: 1}, testProbe()), protocol.CodeBackendIdentityMismatch)
}

func TestHealth_BoundsAndClosesResponseBody(t *testing.T) {
	body := strings.Repeat("x", maxHealthBodyBytes+1)
	tracked := &trackingBody{Reader: strings.NewReader(body)}
	rt := &sequenceTransport{responses: []transportResult{{response: &http.Response{StatusCode: http.StatusOK, Body: tracked}}}}
	assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), testProbe()), protocol.CodeBackendHealthInvalid)
	if !tracked.closed {
		t.Fatal("response body was not closed")
	}

	valid := healthBody("ready", "", 1, "v5.4.0", testCommit)
	valid = strings.Replace(valid, `"backgroundError":null`, `"backgroundError":""`, 1)
	valid += strings.Repeat(" ", maxHealthBodyBytes-len(valid))
	validTransport := &sequenceTransport{responses: []transportResult{{response: jsonResponse(valid)}, {response: jsonResponse(valid)}}}
	if err := testChecker(validTransport).Check(t.Context(), managedExpectation(), testProbe()); err != nil {
		t.Fatalf("exact 64 KiB Check() error = %v, want nil", err)
	}
}

func TestHealth_LocalCancelTakesPriorityOverBackendCancelled(t *testing.T) {
	t.Run("before_request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("cancelled", "", 1, "v5.4.0", testCommit))}}}
		err := testChecker(rt).Check(ctx, managedExpectation(), testProbe())
		assertHealthCode(t, err, protocol.CodeOperationCancelled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Check() error = %v, want errors.Is(context.Canceled)", err)
		}
	})

	t.Run("while_reading_cancelled_response", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		body := newBlockingBody(healthBody("cancelled", "", 1, "v5.4.0", testCommit))
		rt := &sequenceTransport{responses: []transportResult{{response: &http.Response{StatusCode: http.StatusOK, Body: body}}}}
		result := make(chan error, 1)
		go func() { result <- testChecker(rt).Check(ctx, managedExpectation(), testProbe()) }()
		<-body.started
		cancel()
		err := <-result
		assertHealthCode(t, err, protocol.CodeOperationCancelled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Check() error = %v, want errors.Is(context.Canceled)", err)
		}
		if !body.isClosed() {
			t.Fatal("cancel did not close the response body")
		}
		if got := rt.calls(); got != 1 {
			t.Fatalf("RoundTrip calls = %d, want 1", got)
		}
	})

	t.Run("while_probing_and_exited", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		probe := &barrierProbe{started: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
		rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
		result := make(chan error, 1)
		go func() { result <- testChecker(rt).Check(ctx, managedExpectation(), probe) }()
		<-probe.started
		cancel()
		close(probe.exited)
		assertHealthCode(t, <-result, protocol.CodeOperationCancelled)
	})
}

func managedExpectation() Expectation {
	return Expectation{Mode: ModeManaged, Protocol: 1, Version: "v5.4.0", Commit: testCommit}
}

func testChecker(rt http.RoundTripper) *Checker {
	return NewChecker(Config{
		Transport:            rt,
		Clock:                pollingClock{},
		TotalTimeout:         250 * time.Millisecond,
		RequestTimeout:       25 * time.Millisecond,
		PollInterval:         time.Millisecond,
		ConsecutiveSuccesses: 2,
	})
}

type pollingClock struct{}

func (pollingClock) Now() time.Time { return time.Time{} }

func (pollingClock) NewTimer(duration time.Duration) Timer {
	channel := make(chan time.Time, 1)
	if duration == time.Millisecond {
		channel <- time.Time{}
	}
	return staticTimer{channel: channel}
}

type staticTimer struct {
	channel <-chan time.Time
}

func (t staticTimer) C() <-chan time.Time { return t.channel }

func (staticTimer) Stop() bool { return true }

func healthBody(status, backgroundError string, protocolVersion int, version, commit string) string {
	errorJSON := "null"
	if backgroundError != "" {
		errorJSON = fmt.Sprintf("%q", backgroundError)
	}
	return fmt.Sprintf(`{"ready":true,"backgroundStatus":%q,"backgroundError":%s,"protocol":%d,"version":%q,"commit":%q}`, status, errorJSON, protocolVersion, version, commit)
}

func removeJSONField(body, field string) string {
	// 固定测试夹具只需覆盖已知字段，避免测试依赖通用 JSON 重排。
	needle := map[string]string{
		"ready":            `"ready":true,`,
		"backgroundStatus": `"backgroundStatus":"ready",`,
		"backgroundError":  `"backgroundError":null,`,
		"protocol":         `"protocol":1,`,
		"version":          `"version":"v5.4.0",`,
		"commit":           `,"commit":"` + testCommit + `"`,
	}[field]
	return strings.Replace(body, needle, "", 1)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func assertHealthCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("Check() error = nil, want code %s", want)
	}
	var healthErr *Error
	if !errors.As(err, &healthErr) {
		t.Fatalf("Check() error = %T %v, want *Error", err, err)
	}
	if got := healthErr.Code(); got != want {
		t.Fatalf("Check() code = %s, want %s", got, want)
	}
	if got := healthErr.Stage(); got != protocol.StageBackendHealth {
		t.Fatalf("Check() stage = %s, want %s", got, protocol.StageBackendHealth)
	}
}

func assertDetailsOmit(t *testing.T, err error, raw string) {
	t.Helper()
	var healthErr *Error
	if !errors.As(err, &healthErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if strings.Contains(fmt.Sprint(healthErr.Details()), raw) {
		t.Fatalf("Error.Details() contains dynamic value %q: %#v", raw, healthErr.Details())
	}
}

type transportResult struct {
	response *http.Response
	err      error
}

type sequenceTransport struct {
	mu        sync.Mutex
	responses []transportResult
	count     int
	lastURL   string
}

func (t *sequenceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.count++
	t.lastURL = request.URL.String()
	if len(t.responses) == 0 {
		return nil, errors.New("unexpected request")
	}
	result := t.responses[0]
	t.responses = t.responses[1:]
	return result.response, result.err
}

func (t *sequenceTransport) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

type blockingTransport struct {
	started   chan struct{}
	startOnce sync.Once
}

type barrierTransport struct {
	started  chan struct{}
	release  chan struct{}
	response *http.Response
}

func (t *barrierTransport) RoundTrip(*http.Request) (*http.Response, error) {
	close(t.started)
	<-t.release
	return t.response, nil
}

type barrierProbe struct {
	started chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (p *barrierProbe) Exited() <-chan struct{} { return p.exited }

func (p *barrierProbe) Healthy(ctx context.Context) (bool, error) {
	close(p.started)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-p.release:
		return true, nil
	}
}

func (t *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.started != nil {
		t.startOnce.Do(func() { close(t.started) })
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type fakeProbe struct {
	exited   chan struct{}
	healthy  bool
	probeErr error
	calls    int
}

func testProbe() *fakeProbe {
	return &fakeProbe{exited: make(chan struct{}), healthy: true}
}

func (p *fakeProbe) Exited() <-chan struct{} { return p.exited }

func (p *fakeProbe) Healthy(context.Context) (bool, error) {
	p.calls++
	if p.probeErr != nil {
		return false, p.probeErr
	}
	return p.healthy, nil
}

type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

type blockingBody struct {
	payload   string
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockingBody(payload string) *blockingBody {
	return &blockingBody{payload: payload, started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingBody) Read(buffer []byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	if b.payload == "" {
		return 0, io.ErrClosedPipe
	}
	payload := b.payload
	b.payload = ""
	return copy(buffer, payload), io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *blockingBody) isClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

type manualClock struct {
	mu      sync.Mutex
	timers  []*manualTimer
	created chan time.Duration
}

func newManualClock() *manualClock {
	return &manualClock{created: make(chan time.Duration, 16)}
}

func (*manualClock) Now() time.Time { return time.Time{} }

func (c *manualClock) NewTimer(duration time.Duration) Timer {
	timer := &manualTimer{duration: duration, channel: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.created <- duration
	return timer
}

func (c *manualClock) WaitForTimer(t *testing.T, duration time.Duration) {
	t.Helper()
	for {
		select {
		case created := <-c.created:
			if created == duration {
				return
			}
		case <-t.Context().Done():
			t.Fatalf("timer %s was not created: %v", duration, t.Context().Err())
		}
	}
}

func (c *manualClock) Fire(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		timer.mu.Lock()
		if timer.duration == duration && !timer.fired {
			timer.fired = true
			timer.channel <- time.Time{}
			timer.mu.Unlock()
			return
		}
		timer.mu.Unlock()
	}
}

type manualTimer struct {
	mu       sync.Mutex
	duration time.Duration
	channel  chan time.Time
	fired    bool
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }

func (t *manualTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fired {
		return false
	}
	t.fired = true
	return true
}
