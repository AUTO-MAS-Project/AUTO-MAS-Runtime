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

func TestHealth_DefaultReadinessPolling(t *testing.T) {
	clock := newManualClock()
	rt := &sequenceTransport{responses: []transportResult{
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
		{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))},
	}}
	checker := NewChecker(Config{Clock: clock, Transport: rt})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- checker.Check(ctx, managedExpectation(), testProbe())
	}()
	t.Cleanup(func() {
		cancel()
		<-joined
	})
	for _, want := range []time.Duration{60 * time.Second, 2 * time.Second, 200 * time.Millisecond} {
		select {
		case got := <-clock.created:
			if got != want {
				t.Fatalf("timer duration = %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timer %s was not created", want)
		}
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("requests before polling timer = %d, want 1", got)
	}
	select {
	case err := <-done:
		t.Fatalf("Check() returned after one success: %v, want pending", err)
	default:
	}
	clock.Fire(200 * time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Check() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check() did not finish after two successes")
	}
	if got := rt.calls(); got != 2 {
		t.Fatalf("requests at readiness = %d, want 2", got)
	}
}

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

// 明确的否定结果（探针判定身份无效）仍然立即失败；探针**错误**改为连续多次才失败，
// 见 TestHealth_ProbeErrorIsToleratedUntilThreshold。
func TestHealth_JobProbeFailureIsImmediate(t *testing.T) {
	probe := testProbe()
	probe.healthy = false
	rt := &sequenceTransport{responses: []transportResult{{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
	assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), probe), protocol.CodeBackendHealthInvalid)
	if probe.calls != 1 {
		t.Fatalf("身份无效必须立即失败，探针调用 = %d，want 1", probe.calls)
	}
}

// TestHealth_ProbeErrorIsToleratedUntilThreshold 锁定 C15：Job 快照带错误返回多半是
// 过渡态（后端启动期的短命子进程恰在两次系统查询之间退出），单次就判失败会让约四分之一
// 的正常启动报 BACKEND_HEALTH_INVALID。连续 maxConsecutiveProbeErrors 次才判失败，
// 中间任何一次成功都清零。
func TestHealth_ProbeErrorIsToleratedUntilThreshold(t *testing.T) {
	t.Run("错误后恢复则通过", func(t *testing.T) {
		probe := testProbe()
		probe.errLimit = maxConsecutiveProbeErrors - 1
		probe.probeErr = errors.New("job snapshot failed")
		rt := &sequenceTransport{responses: readyResponses(maxConsecutiveProbeErrors - 1 + 2)}
		if err := testChecker(rt).Check(t.Context(), managedExpectation(), probe); err != nil {
			t.Fatalf("阈值以内的探针错误应被容忍，got %v", err)
		}
	})

	t.Run("连续达到阈值才失败", func(t *testing.T) {
		probe := testProbe()
		probe.errLimit = maxConsecutiveProbeErrors
		probe.probeErr = errors.New("job snapshot failed")
		rt := &sequenceTransport{responses: readyResponses(maxConsecutiveProbeErrors)}
		assertHealthCode(t, testChecker(rt).Check(t.Context(), managedExpectation(), probe), protocol.CodeBackendHealthInvalid)
		if probe.calls != maxConsecutiveProbeErrors {
			t.Fatalf("应恰好在第 %d 次错误后失败，探针调用 = %d", maxConsecutiveProbeErrors, probe.calls)
		}
	})
}

func readyResponses(n int) []transportResult {
	results := make([]transportResult, 0, n)
	for range n {
		results = append(results, transportResult{response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))})
	}
	return results
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
	// errLimit 为正时，只有前这么多次探针返回 probeErr，之后恢复正常；
	// 为 0 时 probeErr 一直生效（沿用既有用法）。
	errLimit    int
	errsEmitted int
	calls       int
}

func testProbe() *fakeProbe {
	return &fakeProbe{exited: make(chan struct{}), healthy: true}
}

func (p *fakeProbe) Exited() <-chan struct{} { return p.exited }

func (p *fakeProbe) Healthy(context.Context) (bool, error) {
	p.calls++
	if p.probeErr != nil && (p.errLimit == 0 || p.errsEmitted < p.errLimit) {
		p.errsEmitted++
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

// TestHealth_RequestURLFollowsExpectationPort 锁定增补 1 C12：健康检查地址由
// Expectation.Port 派生，零值回退缺省端口，因此既有调用方行为不变。
func TestHealth_RequestURLFollowsExpectationPort(t *testing.T) {
	tests := []struct {
		name string
		port int
		want string
	}{
		{name: "zero falls back to the default port", port: 0, want: HealthURL},
		{name: "development default", port: 36164, want: "http://127.0.0.1:36164/api/core/health"},
		{name: "explicit port", port: 5555, want: "http://127.0.0.1:5555/api/core/health"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready := jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))
			rt := &sequenceTransport{responses: []transportResult{{response: ready}, {response: jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit))}}}
			expected := managedExpectation()
			expected.Port = test.port
			if err := testChecker(rt).Check(t.Context(), expected, testProbe()); err != nil {
				t.Fatalf("Check() error = %v, want nil", err)
			}
			if got := rt.lastURL; got != test.want {
				t.Fatalf("request URL = %q, want %q", got, test.want)
			}
		})
	}
	if got, want := HealthURL, HealthURLForPort(DefaultPort); got != want {
		t.Fatalf("HealthURL = %q, want the derived default %q", got, want)
	}
	if got := BaseURL(DefaultPort); got != "http://127.0.0.1:36163" || strings.HasSuffix(got, "/") {
		t.Fatalf("BaseURL(DefaultPort) = %q, want no trailing slash", got)
	}
}

// TestHealth_BackgroundBudgetTimeline 锁定增补 2 C21：60 秒内必须拿到第一次有效应答；
// 之后截止时间为 min(开始 + 150 秒, 最后一次有效应答 + 60 秒)，其余立即失败路径不变。
// 全部用默认配置（60s / 150s / 60s / 2s / 200ms）在虚拟时钟上逐步推演，断言精确的收口时刻。
func TestHealth_BackgroundBudgetTimeline(t *testing.T) {
	tests := []struct {
		name        string
		phase       func(elapsed time.Duration) backendPhase
		wantCode    protocol.Code // 为空表示就绪成功
		wantElapsed time.Duration
	}{
		{
			name:        "a_never_reachable_times_out_at_60s",
			phase:       func(time.Duration) backendPhase { return phaseRefused },
			wantCode:    protocol.CodeBackendHealthTimeout,
			wantElapsed: 60 * time.Second,
		},
		{
			name:        "a_requests_never_answer_time_out_at_60s",
			phase:       func(time.Duration) backendPhase { return phaseHang },
			wantCode:    protocol.CodeBackendHealthTimeout,
			wantElapsed: 60 * time.Second,
		},
		{
			name: "b_running_from_5s_ready_at_90s_succeeds",
			phase: func(elapsed time.Duration) backendPhase {
				switch {
				case elapsed < 5*time.Second:
					return phaseRefused
				case elapsed < 90*time.Second:
					return phaseRunning
				default:
					return phaseReady
				}
			},
			wantElapsed: 90*time.Second + 200*time.Millisecond,
		},
		{
			name:        "c_running_forever_times_out_at_150s",
			phase:       func(time.Duration) backendPhase { return phaseRunning },
			wantCode:    protocol.CodeBackendHealthTimeout,
			wantElapsed: 150 * time.Second,
		},
		{
			name:        "d_unreachable_after_10s_running_times_out_at_70s",
			phase:       runningUntil(10*time.Second, phaseRefused),
			wantCode:    protocol.CodeBackendHealthTimeout,
			wantElapsed: 70 * time.Second,
		},
		{
			name: "e_process_exit_during_extension_is_immediate",
			phase: func(elapsed time.Duration) backendPhase {
				if elapsed < 100*time.Second {
					return phaseRunning
				}
				return phaseExit
			},
			wantCode:    protocol.CodeBackendExitedBeforeReady,
			wantElapsed: 100 * time.Second,
		},
		{
			name: "f_background_failed_during_extension_is_immediate",
			phase: func(elapsed time.Duration) backendPhase {
				if elapsed < 100*time.Second {
					return phaseRunning
				}
				return phaseFailed
			},
			wantCode:    protocol.CodeBackendHealthInvalid,
			wantElapsed: 100 * time.Second,
		},
		{
			// 每次挂起的请求耗满 2 秒单请求超时；截止时间 70 秒落在一次挂起中途，必须恰好在 70 秒收口。
			name:        "g_hung_requests_after_10s_time_out_at_70s",
			phase:       runningUntil(10*time.Second, phaseHang),
			wantCode:    protocol.CodeBackendHealthTimeout,
			wantElapsed: 70 * time.Second,
		},
		{
			// 58.6 秒发出的请求挂起跨过 60 秒：第一段的截止时间不得截断它；恢复应答后重新延长直到就绪。
			name: "g_hung_requests_across_60s_then_ready_succeeds",
			phase: func(elapsed time.Duration) backendPhase {
				switch {
				case elapsed <= 10*time.Second:
					return phaseRunning
				case elapsed < 60*time.Second:
					return phaseHang
				case elapsed < 120*time.Second:
					return phaseRunning
				default:
					return phaseReady
				}
			},
			wantElapsed: 120*time.Second + 200*time.Millisecond,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newVirtualClock(200 * time.Millisecond)
			transport := newTimelineTransport(clock, 2*time.Second, test.phase)
			checker := NewChecker(Config{Transport: transport, Clock: clock})
			elapsed, err := runTimeline(t, checker, clock, &fakeProbe{exited: transport.exited, healthy: true})
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("Check() error = %v at %s, want nil", err, elapsed)
				}
			} else {
				assertHealthCode(t, err, test.wantCode)
			}
			if elapsed != test.wantElapsed {
				t.Fatalf("Check() returned at %s, want %s", elapsed, test.wantElapsed)
			}
		})
	}
}

// TestHealth_BackgroundBudgetConfigOverrides 证明两个新时长可经 Config 覆盖（仅供测试使用）。
func TestHealth_BackgroundBudgetConfigOverrides(t *testing.T) {
	tests := []struct {
		name        string
		phase       func(elapsed time.Duration) backendPhase
		wantElapsed time.Duration
	}{
		{name: "silence_window", phase: runningUntil(30*time.Second, phaseRefused), wantElapsed: 75 * time.Second},
		{name: "background_budget", phase: func(time.Duration) backendPhase { return phaseRunning }, wantElapsed: 100 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newVirtualClock(200 * time.Millisecond)
			transport := newTimelineTransport(clock, 2*time.Second, test.phase)
			checker := NewChecker(Config{
				Transport:         transport,
				Clock:             clock,
				BackgroundTimeout: 100 * time.Second,
				SilenceTimeout:    45 * time.Second,
			})
			elapsed, err := runTimeline(t, checker, clock, &fakeProbe{exited: transport.exited, healthy: true})
			assertHealthCode(t, err, protocol.CodeBackendHealthTimeout)
			if elapsed != test.wantElapsed {
				t.Fatalf("Check() returned at %s, want %s", elapsed, test.wantElapsed)
			}
		})
	}
}

// TestHealth_ValidResponseNeverShortensDeadline 证明后台初始化总预算按 TotalTimeout 兜底：
// 配置成 TotalTimeout 大于 BackgroundTimeout 时，有效应答不得把截止时间从第一段提前。
func TestHealth_ValidResponseNeverShortensDeadline(t *testing.T) {
	clock := newVirtualClock(200 * time.Millisecond)
	transport := newTimelineTransport(clock, 2*time.Second, func(elapsed time.Duration) backendPhase {
		if elapsed < 5*time.Second {
			return phaseRefused
		}
		return phaseRunning
	})
	checker := NewChecker(Config{
		Transport:         transport,
		Clock:             clock,
		TotalTimeout:      200 * time.Second,
		BackgroundTimeout: 150 * time.Second,
	})
	elapsed, err := runTimeline(t, checker, clock, &fakeProbe{exited: transport.exited, healthy: true})
	assertHealthCode(t, err, protocol.CodeBackendHealthTimeout)
	if want := 200 * time.Second; elapsed != want {
		t.Fatalf("Check() returned at %s, want %s", elapsed, want)
	}
}

func runningUntil(last time.Duration, after backendPhase) func(time.Duration) backendPhase {
	return func(elapsed time.Duration) backendPhase {
		if elapsed <= last {
			return phaseRunning
		}
		return after
	}
}

// runTimeline 在虚拟时钟上跑完一次 Check，返回收口时的虚拟耗时。
func runTimeline(t *testing.T, checker *Checker, clock *virtualClock, probe Probe) (time.Duration, error) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- checker.Check(t.Context(), managedExpectation(), probe) }()
	select {
	case err := <-result:
		return clock.elapsed(), err
	case <-time.After(30 * time.Second):
		t.Fatalf("Check() did not finish on the virtual timeline, stuck at %s", clock.elapsed())
		return 0, nil
	}
}

// backendPhase 是时间线夹具在某一虚拟时刻对健康请求的应答方式。
type backendPhase uint8

const (
	phaseRefused backendPhase = iota
	phaseHang
	phaseRunning
	phaseReady
	phaseFailed
	phaseExit
)

// timelineTransport 按发出请求时的虚拟耗时决定应答：挂起的请求让虚拟时间前进一个单请求超时，
// 再等待检查器取消它；phaseExit 关闭进程退出信号并以连接失败应答。
type timelineTransport struct {
	clock          *virtualClock
	requestTimeout time.Duration
	phase          func(elapsed time.Duration) backendPhase
	exited         chan struct{}
	exitOnce       sync.Once
}

func newTimelineTransport(clock *virtualClock, requestTimeout time.Duration, phase func(time.Duration) backendPhase) *timelineTransport {
	return &timelineTransport{clock: clock, requestTimeout: requestTimeout, phase: phase, exited: make(chan struct{})}
}

func (t *timelineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch t.phase(t.clock.elapsed()) {
	case phaseHang:
		t.clock.advanceAfterTimer(t.requestTimeout)
		<-request.Context().Done()
		return nil, request.Context().Err()
	case phaseRunning:
		return jsonResponse(healthBody("running", "", 1, "v5.4.0", testCommit)), nil
	case phaseReady:
		return jsonResponse(healthBody("ready", "", 1, "v5.4.0", testCommit)), nil
	case phaseFailed:
		return jsonResponse(healthBody("failed", "", 1, "v5.4.0", testCommit)), nil
	case phaseExit:
		t.exitOnce.Do(func() { close(t.exited) })
		return nil, errors.New("connection refused")
	default:
		return nil, errors.New("connection refused")
	}
}

// virtualEpoch 是虚拟时间线的起点（取自 issue #1227 的 supervise 时刻，仅为可读）。
var virtualEpoch = time.Date(2026, 10, 6, 15, 43, 26, 0, time.UTC)

// virtualClock 是只在两种时刻前进的假时钟：检查器创建轮询计时器时（等待即时间流逝），
// 以及挂起的请求让时间走过一个单请求超时时。前进途中只触发最早到期的一个计时器
// （同时到期取先创建者），并停在它的到期时刻，因此收口时刻可以精确断言。
type virtualClock struct {
	mu sync.Mutex
	// created 在每次创建计时器时广播，与 mu 配对。
	created *sync.Cond
	// 以下字段由 mu 保护。
	now    time.Time
	poll   time.Duration
	timers []*virtualTimer
}

func newVirtualClock(poll time.Duration) *virtualClock {
	clock := &virtualClock{now: virtualEpoch, poll: poll}
	clock.created = sync.NewCond(&clock.mu)
	return clock
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) elapsed() time.Duration {
	return c.Now().Sub(virtualEpoch)
}

func (c *virtualClock) NewTimer(duration time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &virtualTimer{clock: c, deadline: c.now.Add(duration), channel: make(chan time.Time, 1)}
	live := c.timers[:0]
	for _, existing := range c.timers {
		if !existing.done {
			live = append(live, existing)
		}
	}
	c.timers = append(live, timer)
	if duration == c.poll {
		c.advanceLocked(duration)
	}
	c.created.Broadcast()
	return timer
}

// advanceAfterTimer 先等检查器为当前请求建好单请求计时器，再让时间前进 duration。
func (c *virtualClock) advanceAfterTimer(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := c.now.Add(duration)
	for !c.hasLiveTimerAtLocked(want) {
		c.created.Wait()
	}
	c.advanceLocked(duration)
}

func (c *virtualClock) hasLiveTimerAtLocked(deadline time.Time) bool {
	for _, timer := range c.timers {
		if !timer.done && timer.deadline.Equal(deadline) {
			return true
		}
	}
	return false
}

func (c *virtualClock) advanceLocked(duration time.Duration) {
	target := c.now.Add(duration)
	var due *virtualTimer
	for _, timer := range c.timers {
		if timer.done || timer.deadline.After(target) {
			continue
		}
		if due == nil || timer.deadline.Before(due.deadline) {
			due = timer
		}
	}
	if due == nil {
		c.now = target
		return
	}
	if due.deadline.After(c.now) {
		c.now = due.deadline
	}
	due.done = true
	due.channel <- c.now
}

type virtualTimer struct {
	clock    *virtualClock
	deadline time.Time
	channel  chan time.Time
	// done 由 clock.mu 保护：已触发或已停止的计时器不再参与前进。
	done bool
}

func (t *virtualTimer) C() <-chan time.Time { return t.channel }

func (t *virtualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasLive := !t.done
	t.done = true
	return wasLive
}

// TestHealth_RejectsOutOfRangePort 证明越界端口在发出任何请求之前失败关闭。
func TestHealth_RejectsOutOfRangePort(t *testing.T) {
	for _, port := range []int{1023, 65536, -1} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			rt := &sequenceTransport{}
			expected := managedExpectation()
			expected.Port = port
			assertHealthCode(t, testChecker(rt).Check(t.Context(), expected, testProbe()), protocol.CodeBackendHealthInvalid)
			if rt.count != 0 {
				t.Fatalf("request count = %d, want 0", rt.count)
			}
		})
	}
}
