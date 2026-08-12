package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

type recordingSentryTransport struct {
	mu      sync.Mutex
	options sentry.ClientOptions
	events  []*sentry.Event
	flushes int
	closes  int
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (t *recordingSentryTransport) Configure(options sentry.ClientOptions) {
	t.mu.Lock()
	t.options = options
	t.mu.Unlock()
}

func (t *recordingSentryTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()
}

func (t *recordingSentryTransport) Flush(time.Duration) bool {
	t.mu.Lock()
	t.flushes++
	t.mu.Unlock()
	return true
}

func (t *recordingSentryTransport) FlushWithContext(context.Context) bool {
	t.mu.Lock()
	t.flushes++
	t.mu.Unlock()
	return true
}

func (t *recordingSentryTransport) Close() {
	t.mu.Lock()
	t.closes++
	t.mu.Unlock()
}

func (t *recordingSentryTransport) snapshot() (sentry.ClientOptions, []*sentry.Event, int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := append([]*sentry.Event(nil), t.events...)
	return t.options, events, t.flushes, t.closes
}

func newSentryTestProvider(t *testing.T) (*sentryProvider, *recordingSentryTransport) {
	t.Helper()
	transport := &recordingSentryTransport{}
	config := Config{
		Enabled:           true,
		SentryDSN:         "https://public@example.invalid/1",
		SentryEnvironment: "staging",
		SentryRelease:     "runtime@v1.2.3",
		FlushTimeout:      time.Second,
	}
	candidate, err := newSentryProviderWithTransport(config, transport)
	if err != nil {
		t.Fatalf("newSentryProviderWithTransport() error = %v", err)
	}
	provider, ok := candidate.(*sentryProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *sentryProvider", candidate)
	}
	t.Cleanup(func() { provider.close(context.Background()) })
	return provider, transport
}

func validSentryObservation(panic bool) InternalObservation {
	return InternalObservation{
		Command:         "backend supervise",
		Stage:           "backend.spawn",
		Code:            "INTERNAL_ERROR",
		RuntimeVersion:  "runtime@v1.2.3",
		ProtocolVersion: 1,
		Platform:        "windows/amd64",
		Panic:           panic,
	}
}

func TestSentry_CapturesOnlyInternalError(t *testing.T) {
	provider, transport := newSentryTestProvider(t)
	provider.captureInternal(validSentryObservation(false))
	invalid := validSentryObservation(false)
	invalid.Code = "UNEXPECTED"
	provider.captureInternal(invalid)

	_, events, _, _ := transport.snapshot()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Message != sentryInternalMessage {
		t.Fatalf("message = %q, want %q", event.Message, sentryInternalMessage)
	}
	if event.Level != sentry.LevelError {
		t.Fatalf("level = %q, want %q", event.Level, sentry.LevelError)
	}
	if event.Tags[sentryObservationTag] != "" {
		t.Fatalf("internal marker leaked into tags: %#v", event.Tags)
	}
	if event.Tags["command"] != "backend supervise" || event.Tags["code"] != "INTERNAL_ERROR" {
		t.Fatalf("stable tags = %#v", event.Tags)
	}
	if len(event.Exception) != 1 || event.Exception[0].Value != sentryInternalMessage {
		t.Fatalf("exception = %#v, want one fixed exception", event.Exception)
	}
}

func TestSentry_CapturesUnexpectedPanicWithoutPanicValue(t *testing.T) {
	provider, transport := newSentryTestProvider(t)
	provider.captureInternal(validSentryObservation(true))

	_, events, _, _ := transport.snapshot()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Message != sentryPanicMessage || event.Level != sentry.LevelFatal {
		t.Fatalf("panic event message/level = %q/%q", event.Message, event.Level)
	}
	if strings.Contains(event.Message, "secret panic") {
		t.Fatal("panic value leaked into message")
	}
	if len(event.Exception) != 1 || event.Exception[0].Mechanism == nil {
		t.Fatalf("panic exception mechanism = %#v, want unhandled mechanism", event.Exception)
	}
	if event.Exception[0].Mechanism.Handled == nil || *event.Exception[0].Mechanism.Handled {
		t.Fatalf("panic handled flag = %v, want false", event.Exception[0].Mechanism.Handled)
	}
}

func TestSentry_BeforeSendRemovesPIIAndRawText(t *testing.T) {
	_, transport := newSentryTestProvider(t)
	options, _, _, _ := transport.snapshot()
	if options.BeforeSend == nil {
		t.Fatal("BeforeSend is nil")
	}
	rawPath := `C:\Users\alice\workspace\secret`
	event := sentry.NewEvent()
	event.Tags = map[string]string{
		sentryObservationTag: "internal_error",
		"command":            rawPath,
		"stage":              "backend_spawn",
		"hostname":           "alice-pc",
	}
	event.Message = "raw panic value: super-secret"
	event.ServerName = "alice-pc"
	event.User = sentry.User{Username: "alice"}
	event.Breadcrumbs = []*sentry.Breadcrumb{{Message: "raw breadcrumb"}}
	event.Contexts = map[string]sentry.Context{"raw": {"value": "secret"}}
	event.Request = &sentry.Request{URL: "https://example.invalid/C:/Users/alice"}
	event.Exception = []sentry.Exception{{
		Type:  "raw error type",
		Value: "raw panic value: super-secret",
		Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
			Function:    "runtime.fn",
			Module:      "runtime",
			Filename:    rawPath + `\main.go`,
			AbsPath:     rawPath + `\main.go`,
			ContextLine: "secret source line",
			Vars:        map[string]interface{}{"password": "secret"},
			Lineno:      42,
		}}},
	}}

	sanitized := options.BeforeSend(event, nil)
	if sanitized == nil {
		t.Fatal("BeforeSend unexpectedly dropped target event")
	}
	payload, err := json.Marshal(sanitized)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(payload)
	for _, secret := range []string{rawPath, "alice-pc", "alice", "raw panic value", "raw breadcrumb", "secret source line"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("sanitized payload contains %q: %s", secret, serialized)
		}
	}
	if sanitized.ServerName != "" || sanitized.Request != nil || !sanitized.User.IsEmpty() || sanitized.Breadcrumbs != nil || sanitized.Contexts != nil {
		t.Fatalf("PII fields remain: server=%q request=%#v user=%#v breadcrumbs=%#v contexts=%#v", sanitized.ServerName, sanitized.Request, sanitized.User, sanitized.Breadcrumbs, sanitized.Contexts)
	}
}

func TestSentry_SanitizesStackFramePaths(t *testing.T) {
	event := sentry.NewEvent()
	event.Tags = map[string]string{sentryObservationTag: sentryObservationInternal}
	event.Exception = []sentry.Exception{{Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{
		{Function: "safeFunction", Module: "github.com/example/runtime", Filename: `C:\Users\alice\main.go`, AbsPath: `C:\Users\alice\main.go`, Lineno: 7},
		{Function: `C:\Users\alice\privateFunction`, Module: `/Users/alice/private`, Filename: `/Users/alice/main.go`, AbsPath: `/Users/alice/main.go`, Lineno: 8},
	}}}}

	sanitized := sanitizeSentryEvent(event, "runtime@v1.2.3", "staging")
	if sanitized == nil || len(sanitized.Exception) != 1 || sanitized.Exception[0].Stacktrace == nil {
		t.Fatalf("sanitized stacktrace missing: %#v", sanitized)
	}
	frames := sanitized.Exception[0].Stacktrace.Frames
	if len(frames) != 2 {
		t.Fatalf("frame count = %d, want 2", len(frames))
	}
	if frames[0].Function != "safeFunction" || frames[0].Module != "github.com/example/runtime" || frames[0].Lineno != 7 {
		t.Fatalf("safe frame = %#v", frames[0])
	}
	if frames[0].Filename != "" || frames[0].AbsPath != "" {
		t.Fatalf("safe frame path fields not cleared: %#v", frames[0])
	}
	if frames[1].Function != "" || frames[1].Module != "" || frames[1].Filename != "" || frames[1].AbsPath != "" {
		t.Fatalf("path-like frame leaked: %#v", frames[1])
	}
}

func TestSentry_LimitsSanitizedStackFrames(t *testing.T) {
	frames := make([]sentry.Frame, maxSentryStackFrames+8)
	for index := range frames {
		frames[index] = sentry.Frame{Function: "runtime.function", Lineno: index + 1}
	}
	sanitized := sanitizeSentryStacktrace(&sentry.Stacktrace{Frames: frames})
	if sanitized == nil {
		t.Fatal("sanitizeSentryStacktrace() = nil, want bounded frames")
	}
	if len(sanitized.Frames) != maxSentryStackFrames {
		t.Fatalf("sanitized frame count = %d, want %d", len(sanitized.Frames), maxSentryStackFrames)
	}
}

func TestSentry_HTTPClientRestrictsEnvelopeEndpoint(t *testing.T) {
	client, err := newSentryHTTPClient("https://public@sentry.example.test/42", time.Second)
	if err != nil {
		t.Fatalf("newSentryHTTPClient() error = %v", err)
	}
	if client.Timeout != DefaultFlushTimeout {
		t.Fatalf("HTTP timeout = %s, want %s", client.Timeout, DefaultFlushTimeout)
	}
	transport, ok := client.Transport.(*sentryEndpointTransport)
	if !ok {
		t.Fatalf("transport type = %T, want *sentryEndpointTransport", client.Transport)
	}
	if transport.allowedHost != "sentry.example.test" || transport.allowedPath != "/api/42/envelope/" {
		t.Fatalf("allowed endpoint = https://%s%s, want configured envelope endpoint", transport.allowedHost, transport.allowedPath)
	}
	redirectErr := client.CheckRedirect(&http.Request{}, nil)
	if !errors.Is(redirectErr, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v, want http.ErrUseLastResponse", redirectErr)
	}
}

func TestSentry_OnlyAllowsConfiguredEnvelopeEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		url         string
		contentType string
		mutate      func(*url.URL)
		wantAllowed bool
	}{
		{name: "canonical", method: http.MethodPost, url: "https://sentry.example.test/api/42/envelope/", contentType: "application/x-sentry-envelope", wantAllowed: true},
		{name: "query", method: http.MethodPost, url: "https://sentry.example.test/api/42/envelope/?token=secret", contentType: "application/x-sentry-envelope"},
		{name: "force query", method: http.MethodPost, url: "https://sentry.example.test/api/42/envelope/?", contentType: "application/x-sentry-envelope"},
		{name: "userinfo", method: http.MethodPost, url: "https://user@sentry.example.test/api/42/envelope/", contentType: "application/x-sentry-envelope"},
		{name: "different host", method: http.MethodPost, url: "https://attacker.example.test/api/42/envelope/", contentType: "application/x-sentry-envelope"},
		{name: "different path", method: http.MethodPost, url: "https://sentry.example.test/api/7/envelope/", contentType: "application/x-sentry-envelope"},
		{name: "different method", method: http.MethodGet, url: "https://sentry.example.test/api/42/envelope/", contentType: "application/x-sentry-envelope"},
		{name: "different content type", method: http.MethodPost, url: "https://sentry.example.test/api/42/envelope/", contentType: "application/json"},
		{name: "raw path", method: http.MethodPost, url: "https://sentry.example.test/api/42/envelope/", contentType: "application/x-sentry-envelope", mutate: func(value *url.URL) {
			value.RawPath = "/api/42/%65nvelope/"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			transport := &sentryEndpointTransport{
				base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    request,
					}, nil
				}),
				allowedHost: "sentry.example.test",
				allowedPath: "/api/42/envelope/",
			}
			request, err := http.NewRequest(test.method, test.url, strings.NewReader("envelope"))
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			request.Header.Set("Content-Type", test.contentType)
			if test.mutate != nil {
				test.mutate(request.URL)
			}
			response, roundTripErr := transport.RoundTrip(request)
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if test.wantAllowed {
				if roundTripErr != nil || calls != 1 {
					t.Fatalf("RoundTrip() = calls %d, error %v, want allowed", calls, roundTripErr)
				}
				return
			}
			if roundTripErr == nil || calls != 0 {
				t.Fatalf("RoundTrip() = calls %d, error %v, want rejection", calls, roundTripErr)
			}
		})
	}
}

func TestSentry_RejectsRedirect(t *testing.T) {
	transport := &sentryEndpointTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"https://attacker.example.test/api/42/envelope/"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		}),
		allowedHost: "sentry.example.test",
		allowedPath: "/api/42/envelope/",
	}
	request, err := http.NewRequest(http.MethodPost, "https://sentry.example.test/api/42/envelope/", strings.NewReader("envelope"))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/x-sentry-envelope")
	if _, err := transport.RoundTrip(request); err == nil || !strings.Contains(err.Error(), "sentry redirect is not allowed") {
		t.Fatalf("RoundTrip() error = %v, want redirect rejection", err)
	}
}

func TestSentry_DisablesTracingLogsAndDefaultPII(t *testing.T) {
	provider, transport := newSentryTestProvider(t)
	options, _, _, _ := transport.snapshot()
	if provider == nil || options.SendDefaultPII || options.EnableTracing || !options.DisableLogs || !options.DisableMetrics || !options.DisableClientReports || !options.DisableTelemetryBuffer {
		t.Fatalf("privacy options = %+v", options)
	}
	if options.ServerName != sentryServerName || options.Release != "runtime@v1.2.3" || options.Environment != "staging" {
		t.Fatalf("metadata options = server=%q release=%q environment=%q", options.ServerName, options.Release, options.Environment)
	}
	if options.DataCollection == nil || !options.DataCollection.UserInfo.IsSet || options.DataCollection.UserInfo.Value ||
		options.DataCollection.Cookies == nil || options.DataCollection.Cookies.Mode != sentry.CollectionOff ||
		options.DataCollection.HTTPHeaders == nil || options.DataCollection.HTTPHeaders.Request.Mode != sentry.CollectionOff ||
		options.DataCollection.HTTPHeaders.Response.Mode != sentry.CollectionOff || len(options.DataCollection.HTTPBodies) != 0 ||
		options.DataCollection.QueryParams == nil || options.DataCollection.QueryParams.Mode != sentry.CollectionOff {
		t.Fatalf("data collection options = %#v", options.DataCollection)
	}
}

func TestSentry_InvalidEnvironmentAndReleaseFallBackToSafeDefaults(t *testing.T) {
	transport := &recordingSentryTransport{}
	candidate, err := newSentryProviderWithTransport(Config{
		Enabled:           true,
		SentryDSN:         "https://public@example.invalid/1",
		SentryEnvironment: "alice@example.com",
		SentryRelease:     "secret-token",
	}, transport)
	if err != nil {
		t.Fatalf("newSentryProviderWithTransport() error = %v", err)
	}
	provider, ok := candidate.(*sentryProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *sentryProvider", candidate)
	}
	t.Cleanup(func() { provider.close(context.Background()) })

	options, _, _, _ := transport.snapshot()
	if options.Environment != defaultSentryEnvironment || options.Release != defaultSentryRelease {
		t.Fatalf("metadata = environment:%q release:%q, want safe defaults", options.Environment, options.Release)
	}
}

func TestSentry_MissingDSNOfflineAndFailureAreSilent(t *testing.T) {
	if candidate, err := newSentryProvider(Config{Enabled: true}); candidate != nil || err != nil {
		t.Fatalf("missing DSN result = provider=%#v error=%v, want nil/nil", candidate, err)
	}
	if candidate, err := newSentryProvider(Config{Enabled: true, Offline: true, SentryDSN: "https://public@example.invalid/1"}); candidate != nil || err != nil {
		t.Fatalf("offline result = provider=%#v error=%v, want nil/nil", candidate, err)
	}
	if candidate, err := newSentryProvider(Config{Enabled: true, SentryDSN: "not a DSN"}); candidate != nil || err == nil {
		t.Fatalf("invalid DSN result = provider=%#v error=%v, want nil/non-nil", candidate, err)
	}
}

func TestSentry_FlushesBeforeClose(t *testing.T) {
	provider, transport := newSentryTestProvider(t)
	provider.close(context.Background())
	_, _, flushes, closes := transport.snapshot()
	if flushes != 1 || closes != 1 {
		t.Fatalf("flush/close calls = %d/%d, want 1/1", flushes, closes)
	}
}
