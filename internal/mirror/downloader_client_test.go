package mirror

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func trustedClientForServers(
	t *testing.T,
	servers ...*httptest.Server,
) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	for _, server := range servers {
		roots.AddCert(server.Certificate())
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}
	return newHTTPClientWithTransport(transport)
}

func downloaderForClientTest(
	t *testing.T,
	client httpClient,
	timers timerFactory,
) *Downloader {
	t.Helper()
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v, want nil", err)
	}
	dependencies := testDependencies(&fakeSessionFactory{}, client)
	if timers != nil {
		dependencies.timers = timers
	}
	downloader, err := newDownloaderWithDependencies(
		testLayout(t),
		options,
		dependencies,
	)
	if err != nil {
		t.Fatalf("newDownloaderWithDependencies() error = %v, want nil", err)
	}
	return downloader
}

func TestNewDownloader_RejectsNilLayout(t *testing.T) {
	_, err := NewDownloader(nil)
	if !errors.Is(err, ErrInvalidDownloaderOption) {
		t.Fatalf("NewDownloader(nil) error = %v, want ErrInvalidDownloaderOption", err)
	}
}

type productionCleanupValueKey struct{}

func TestProductionCleanupContext_PreservesValuesIgnoresCancellationAndBoundsDeadline(
	t *testing.T,
) {
	operationCtx, operationCancel := context.WithCancel(
		context.WithValue(
			t.Context(),
			productionCleanupValueKey{},
			"preserved",
		),
	)
	operationCancel()
	before := time.Now()
	cleanupCtx, cleanupCancel := productionCleanupContext(operationCtx)
	defer cleanupCancel()
	after := time.Now()

	if got := cleanupCtx.Value(productionCleanupValueKey{}); got != "preserved" {
		t.Fatalf("cleanup context value = %v, want preserved", got)
	}
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context error = %v, want nil", err)
	}
	deadline, ok := cleanupCtx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline")
	}
	if deadline.After(after.Add(cleanupTimeout)) {
		t.Fatalf(
			"cleanup deadline = %s, later than construction + %s",
			deadline,
			cleanupTimeout,
		)
	}
	if deadline.Before(before) {
		t.Fatalf("cleanup deadline = %s, before construction", deadline)
	}
}

func TestDefaultHTTPClient_UsesTLSVerificationIdentityAndNoGlobalTimeout(t *testing.T) {
	client, ok := newDefaultHTTPClient().(*http.Client)
	if !ok {
		t.Fatal("newDefaultHTTPClient() did not return *http.Client")
	}
	if client.Timeout != 0 {
		t.Fatalf("client.Timeout = %s, want 0", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("client.Transport is not *http.Transport")
	}
	if !transport.DisableCompression {
		t.Fatal("DisableCompression = false, want true")
	}
	if transport.TLSClientConfig != nil &&
		transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify = true, want false")
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "abc")
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := client.Do(request)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("default client untrusted TLS error = nil, want non-nil")
	}
}

func TestDownloaderClient_UsesTrustedTLSAndIdentityEncoding(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		requestCount.Add(1)
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		if got := request.Header.Get("Range"); got != "" {
			t.Errorf("Range = %q, want empty", got)
		}
		if got := request.Header.Get("If-Range"); got != "" {
			t.Errorf("If-Range = %q, want empty", got)
		}
		writer.Header().Set("Content-Length", "3")
		_, _ = io.WriteString(writer, "abc")
	}))
	defer server.Close()
	client := trustedClientForServers(t, server)
	downloader := downloaderForClientTest(t, client, nil)
	handle, failure := downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, server.URL+"?signature=do-not-log"),
	)
	if failure != nil {
		t.Fatalf("doRequest() failure = %v, want nil", failure)
	}
	if requestCount.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requestCount.Load())
	}
	handle.cancel()
	if err := handle.response.Body.Close(); err != nil {
		t.Fatalf("Body.Close() error = %v, want nil", err)
	}
}

func TestDownloaderClient_RejectsRedirectDowngradeBeforeRequest(t *testing.T) {
	var downgradeHits atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		downgradeHits.Add(1)
		_, _ = io.WriteString(writer, "must not be reached")
	}))
	defer httpServer.Close()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		http.Redirect(
			writer,
			&http.Request{},
			httpServer.URL+"/asset?signature=redirect-secret",
			http.StatusFound,
		)
	}))
	defer tlsServer.Close()
	client := trustedClientForServers(t, tlsServer)
	downloader := downloaderForClientTest(t, client, nil)
	_, failure := downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, tlsServer.URL+"?origin-secret=yes"),
	)
	if failure == nil || failure.Kind != FailureRedirectDowngrade {
		t.Fatalf("failure = %#v, want FailureRedirectDowngrade", failure)
	}
	if downgradeHits.Load() != 0 {
		t.Fatalf("downgrade request hits = %d, want 0", downgradeHits.Load())
	}
	if strings.Contains(failure.Error()+" "+failure.Err.Error(), "secret") {
		t.Fatalf("public error text leaked URL: %v / %v", failure, failure.Err)
	}
}

func TestDownloaderClient_AllowsHTTPSRedirectAndLimitsTen(t *testing.T) {
	var loopHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "abc")
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path == "/loop" {
			loopHits.Add(1)
			http.Redirect(writer, request, "/loop?signature=loop-secret", http.StatusFound)
			return
		}
		http.Redirect(writer, request, target.URL+"/asset", http.StatusFound)
	}))
	defer redirect.Close()
	client := trustedClientForServers(t, redirect, target)
	downloader := downloaderForClientTest(t, client, nil)

	handle, failure := downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, redirect.URL+"/ok"),
	)
	if failure != nil {
		t.Fatalf("HTTPS redirect failure = %v, want nil", failure)
	}
	handle.cancel()
	if err := handle.response.Body.Close(); err != nil {
		t.Fatalf("redirect Body.Close() error = %v, want nil", err)
	}

	_, failure = downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, redirect.URL+"/loop"),
	)
	if failure == nil || failure.Kind != FailureURLPolicy {
		t.Fatalf("redirect limit failure = %#v, want FailureURLPolicy", failure)
	}
	if loopHits.Load() != maxRedirects+1 {
		t.Fatalf("redirect loop requests = %d, want %d", loopHits.Load(), maxRedirects+1)
	}
	if strings.Contains(failure.Error()+" "+failure.Err.Error(), "loop-secret") {
		t.Fatal("redirect limit error leaked signed query")
	}
}

func TestDownloaderClient_DoResultChannelJoinsAfterCancelOrTimeout(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(context.CancelFunc, *manualTimer)
		kind    FailureKind
	}{
		{
			name: "cancel",
			trigger: func(cancel context.CancelFunc, _ *manualTimer) {
				cancel()
			},
			kind: FailureCancelled,
		},
		{
			name: "connect timeout",
			trigger: func(_ context.CancelFunc, timer *manualTimer) {
				timer.Fire()
			},
			kind: FailureConnectTimeout,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			doStarted := make(chan struct{})
			doExited := make(chan struct{})
			client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
				close(doStarted)
				<-request.Context().Done()
				close(doExited)
				return nil, request.Context().Err()
			}}
			connectTimer := newManualTimer()
			downloader := downloaderForClientTest(
				t,
				client,
				func(time.Duration) timer { return connectTimer },
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan *DownloadFailure, 1)
			go func() {
				_, failure := downloader.doRequest(
					ctx,
					mustHTTPSURL(t, "https://example.invalid/asset"),
				)
				result <- failure
			}()
			waitSignal(t, doStarted)
			testCase.trigger(cancel, connectTimer)
			failure := waitValue(t, result)
			if failure == nil || failure.Kind != testCase.kind {
				t.Fatalf("failure = %#v, want kind %q", failure, testCase.kind)
			}
			waitSignal(t, doExited)
		})
	}
}

func TestDownloaderClient_ConnectTimeoutClassificationFreezesBeforeJoin(t *testing.T) {
	doStarted := make(chan struct{})
	requestCancelled := make(chan struct{})
	releaseDo := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(releaseDo) })
	t.Cleanup(releaseOnce)
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		close(doStarted)
		<-request.Context().Done()
		close(requestCancelled)
		<-releaseDo
		return nil, request.Context().Err()
	}}
	connectTimer := newManualTimer()
	downloader := downloaderForClientTest(
		t,
		client,
		func(time.Duration) timer { return connectTimer },
	)
	operationCtx, cancelOperation := context.WithCancel(t.Context())
	defer cancelOperation()
	result := make(chan *DownloadFailure, 1)
	go func() {
		_, failure := downloader.doRequest(
			operationCtx,
			mustHTTPSURL(t, "https://example.invalid/asset"),
		)
		result <- failure
	}()
	waitSignal(t, doStarted)
	connectTimer.Fire()
	waitSignal(t, requestCancelled)
	cancelOperation()
	releaseOnce()
	failure := waitValue(t, result)
	if failure == nil || failure.Kind != FailureConnectTimeout {
		t.Fatalf("failure = %#v, want FailureConnectTimeout", failure)
	}
}

func TestDownloaderClient_SanitizesURLErrorAndFinalURL(t *testing.T) {
	typedCause := &testTypedError{cause: errTestSecret}
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		return nil, &url.Error{
			Op:  "Get",
			URL: "https://user:password@example.invalid/asset?token=url-secret#fragment",
			Err: typedCause,
		}
	}}
	downloader := downloaderForClientTest(t, client, nil)
	_, failure := downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, "https://example.invalid/asset?token=request-secret"),
	)
	if failure == nil || failure.Kind != FailureNetwork {
		t.Fatalf("failure = %#v, want FailureNetwork", failure)
	}
	text := failure.Error() + " " + failure.Err.Error()
	for _, secret := range []string{"user", "password", "url-secret", "request-secret", "fragment", "callback-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("public text %q contains %q", text, secret)
		}
	}
	if !errors.Is(failure, errTestSecret) {
		t.Fatal("errors.Is(failure, errTestSecret) = false, want true")
	}
	var typed *testTypedError
	if !errors.As(failure, &typed) {
		t.Fatal("errors.As(failure, *testTypedError) = false, want true")
	}
	var leakedURL *url.Error
	if errors.As(failure, &leakedURL) {
		t.Fatalf("DownloadFailure retained *url.Error: %#v", leakedURL)
	}

	for _, testCase := range []struct {
		name      string
		err       *url.Error
		wantCause error
	}{
		{
			name: "nil cause",
			err: &url.Error{
				Op:  "Get",
				URL: "https://example.invalid/asset?token=nil-secret",
			},
			wantCause: errTransportCauseUnavailable,
		},
		{
			name: "nested url error",
			err: &url.Error{
				Op:  "Get",
				URL: "https://example.invalid/outer?token=outer-secret",
				Err: &url.Error{
					Op:  "Get",
					URL: "https://example.invalid/inner?token=inner-secret",
					Err: typedCause,
				},
			},
			wantCause: typedCause,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
				return nil, testCase.err
			}}
			downloader := downloaderForClientTest(t, client, nil)
			_, gotFailure := downloader.doRequest(
				t.Context(),
				mustHTTPSURL(t, "https://example.invalid/asset"),
			)
			if gotFailure == nil ||
				!errors.Is(gotFailure, testCase.wantCause) {
				t.Fatalf(
					"doRequest() failure = %#v, want cause %v",
					gotFailure,
					testCase.wantCause,
				)
			}
			var gotURL *url.Error
			if errors.As(gotFailure, &gotURL) {
				t.Fatalf("DownloadFailure retained *url.Error: %#v", gotURL)
			}
			if strings.Contains(
				gotFailure.Error()+" "+gotFailure.Err.Error(),
				"secret",
			) {
				t.Fatalf("public error text leaked URL: %v", gotFailure)
			}
		})
	}

	finalURLClient := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("abc")),
			Request: &http.Request{
				URL: mustURL(t, "http://example.invalid/final?token=final-secret"),
			},
		}, nil
	}}
	downloader = downloaderForClientTest(t, finalURLClient, nil)
	_, failure = downloader.doRequest(
		t.Context(),
		mustHTTPSURL(t, "https://example.invalid/asset"),
	)
	if failure == nil || failure.Kind != FailureRedirectDowngrade {
		t.Fatalf("final URL failure = %#v, want FailureRedirectDowngrade", failure)
	}
	if strings.Contains(failure.Error()+" "+failure.Err.Error(), "final-secret") {
		t.Fatal("final response URL leaked query")
	}
}

func mustHTTPSURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := validateHTTPSURL(raw)
	if err != nil {
		t.Fatalf("validateHTTPSURL(%q) error = %v", raw, err)
	}
	return parsed
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return parsed
}

type manualTimer struct {
	channel chan time.Time
	reset   chan struct{}
	stopped atomic.Bool
}

func newManualTimer() *manualTimer {
	return &manualTimer{
		channel: make(chan time.Time, 1),
		reset:   make(chan struct{}, 1),
	}
}

func (t *manualTimer) C() <-chan time.Time {
	return t.channel
}

func (t *manualTimer) Stop() bool {
	return !t.stopped.Swap(true)
}

func (t *manualTimer) Reset(time.Duration) bool {
	t.stopped.Store(false)
	select {
	case t.reset <- struct{}{}:
	default:
	}
	return false
}

func (t *manualTimer) Fire() bool {
	if t.stopped.Load() {
		return false
	}
	t.channel <- time.Unix(2, 0)
	return true
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test signal")
	}
}

func waitValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test value")
		var zero T
		return zero
	}
}
