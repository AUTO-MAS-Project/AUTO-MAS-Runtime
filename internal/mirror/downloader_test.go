package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

var errTestSecret = errors.New("callback-secret?token=raw")

type fakeHTTPClient struct {
	mu      sync.Mutex
	calls   int
	request *http.Request
	do      func(*http.Request) (*http.Response, error)
}

func (c *fakeHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.request = request
	do := c.do
	c.mu.Unlock()
	if do == nil {
		return nil, errors.New("unexpected network call")
	}
	return do(request)
}

func (c *fakeHTTPClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type fakeSessionFactory struct {
	mu      sync.Mutex
	calls   int
	name    string
	session downloadSession
	err     error
}

func (f *fakeSessionFactory) Begin(
	_ context.Context,
	name string,
) (downloadSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.name = name
	return f.session, f.err
}

func (f *fakeSessionFactory) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeDownloadSession struct {
	path       string
	partPath   string
	write      func([]byte) (int, error)
	publish    func(context.Context) (filesystem.PublishResult, error)
	abort      func(context.Context) (filesystem.AbortResult, error)
	abortCalls int
}

func (s *fakeDownloadSession) Write(p []byte) (int, error) {
	if s.write == nil {
		return len(p), nil
	}
	return s.write(p)
}

func (s *fakeDownloadSession) Path() string {
	return s.path
}

func (s *fakeDownloadSession) PartPath() string {
	return s.partPath
}

func (s *fakeDownloadSession) PublishNoReplace(
	ctx context.Context,
) (filesystem.PublishResult, error) {
	if s.publish == nil {
		return filesystem.PublishResult{Published: true}, nil
	}
	return s.publish(ctx)
}

func (s *fakeDownloadSession) Abort(
	ctx context.Context,
) (filesystem.AbortResult, error) {
	s.abortCalls++
	if s.abort == nil {
		return filesystem.AbortResult{Removed: true}, nil
	}
	return s.abort(ctx)
}

type inertTimer struct {
	channel chan time.Time
}

func newInertTimer() *inertTimer {
	return &inertTimer{channel: make(chan time.Time)}
}

func (t *inertTimer) C() <-chan time.Time {
	return t.channel
}

func (t *inertTimer) Stop() bool {
	return true
}

func (t *inertTimer) Reset(time.Duration) bool {
	return false
}

func testLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func testDependencies(
	sessions sessionFactory,
	client httpClient,
) downloaderDependencies {
	return downloaderDependencies{
		sessions: sessions,
		client:   client,
		clock:    func() time.Time { return time.Unix(1, 0) },
		timers:   func(time.Duration) timer { return newInertTimer() },
		cleanup: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.WithoutCancel(ctx))
		},
	}
}

func validRequest() DownloadRequest {
	return DownloadRequest{
		URL:            "https://downloads.example.invalid/uv.zip?signature=secret",
		FileName:       "uv.zip",
		ExpectedSize:   3,
		ExpectedSHA256: strings.Repeat("a", 64),
	}
}

func TestDownloaderFailureKinds_AreValidOpenDiagnostics(t *testing.T) {
	kinds := []FailureKind{
		FailureInvalidRequest,
		FailureURLPolicy,
		FailureRedirectDowngrade,
		FailureConnectTimeout,
		FailureReadTimeout,
		FailureNetwork,
		FailureHTTPStatus,
		FailureSizeMismatch,
		FailureChecksumMismatch,
		FailureCancelled,
		FailureProgress,
		FailureDestinationOccupied,
		FailureFilesystem,
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Errorf("FailureKind(%q).Valid() = false, want true", kind)
		}
	}
	future := FailureKind("future_transport_detail")
	if !future.Valid() {
		t.Fatalf("future FailureKind.Valid() = false, want true")
	}
}

func TestDownloader_ExportedFailureDeclarationsHaveChineseComments(t *testing.T) {
	targets := map[string]bool{
		"FailureInvalidRequest":      false,
		"FailureURLPolicy":           false,
		"FailureRedirectDowngrade":   false,
		"FailureConnectTimeout":      false,
		"FailureReadTimeout":         false,
		"FailureNetwork":             false,
		"FailureHTTPStatus":          false,
		"FailureSizeMismatch":        false,
		"FailureChecksumMismatch":    false,
		"FailureCancelled":           false,
		"FailureProgress":            false,
		"FailureDestinationOccupied": false,
		"FailureFilesystem":          false,
		"ErrInvalidDownloaderOption": false,
	}
	source, err := os.ReadFile("downloader.go")
	if err != nil {
		t.Fatalf("ReadFile(downloader.go) error = %v", err)
	}
	file, err := parser.ParseFile(
		token.NewFileSet(),
		"downloader.go",
		source,
		parser.ParseComments,
	)
	if err != nil {
		t.Fatalf("ParseFile(downloader.go) error = %v", err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if _, tracked := targets[name.Name]; !tracked {
					continue
				}
				comment := ""
				if value.Doc != nil {
					comment += value.Doc.Text()
				}
				if general.Doc != nil {
					comment += general.Doc.Text()
				}
				targets[name.Name] = strings.ContainsFunc(comment, func(r rune) bool {
					return unicode.Is(unicode.Han, r)
				})
			}
		}
	}
	for name, valid := range targets {
		if !valid {
			t.Errorf("%s Chinese doc comment = false, want true", name)
		}
	}
}

func TestNewDownloader_ValidatesOptionsAndDependencies(t *testing.T) {
	validSessions := &fakeSessionFactory{}
	validClient := &fakeHTTPClient{}
	validDependencies := testDependencies(validSessions, validClient)
	cases := []struct {
		name    string
		options []DownloaderOption
		mutate  func(*downloaderDependencies)
	}{
		{name: "nil option", options: []DownloaderOption{nil}},
		{name: "zero connect", options: []DownloaderOption{WithDownloaderTimeouts(0, time.Second)}},
		{name: "zero read", options: []DownloaderOption{WithDownloaderTimeouts(time.Second, 0)}},
		{name: "duplicate timeout", options: []DownloaderOption{
			WithDownloaderTimeouts(time.Second, time.Second),
			WithDownloaderTimeouts(time.Second, time.Second),
		}},
		{name: "zero interval", options: []DownloaderOption{WithDownloaderProgressInterval(0)}},
		{name: "duplicate interval", options: []DownloaderOption{
			WithDownloaderProgressInterval(time.Second),
			WithDownloaderProgressInterval(time.Second),
		}},
		{name: "nil sessions", mutate: func(deps *downloaderDependencies) { deps.sessions = nil }},
		{name: "nil client", mutate: func(deps *downloaderDependencies) { deps.client = nil }},
		{name: "nil clock", mutate: func(deps *downloaderDependencies) { deps.clock = nil }},
		{name: "nil timers", mutate: func(deps *downloaderDependencies) { deps.timers = nil }},
		{name: "nil cleanup", mutate: func(deps *downloaderDependencies) { deps.cleanup = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options, err := resolveDownloaderOptions(testCase.options)
			if testCase.name == "nil option" ||
				strings.Contains(testCase.name, "zero") ||
				strings.Contains(testCase.name, "duplicate") {
				if !errors.Is(err, ErrInvalidDownloaderOption) {
					t.Fatalf("resolveDownloaderOptions() error = %v, want ErrInvalidDownloaderOption", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDownloaderOptions() error = %v, want nil", err)
			}
			dependencies := validDependencies
			testCase.mutate(&dependencies)
			_, err = newDownloaderWithDependencies(
				testLayout(t),
				options,
				dependencies,
			)
			if !errors.Is(err, ErrInvalidDownloaderOption) {
				t.Fatalf("newDownloaderWithDependencies() error = %v, want ErrInvalidDownloaderOption", err)
			}
		})
	}
}

func TestDownloader_RejectsInvalidRequestBeforeBeginOrNetwork(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DownloadRequest)
		kind   FailureKind
	}{
		{name: "nil context", kind: FailureInvalidRequest},
		{name: "relative URL", mutate: func(r *DownloadRequest) { r.URL = "/uv.zip" }, kind: FailureURLPolicy},
		{name: "http loopback", mutate: func(r *DownloadRequest) { r.URL = "http://127.0.0.1/uv.zip" }, kind: FailureURLPolicy},
		{name: "userinfo", mutate: func(r *DownloadRequest) { r.URL = "https://user:secret@example.invalid/uv.zip" }, kind: FailureURLPolicy},
		{name: "fragment", mutate: func(r *DownloadRequest) { r.URL = "https://example.invalid/uv.zip#secret" }, kind: FailureURLPolicy},
		{name: "empty filename", mutate: func(r *DownloadRequest) { r.FileName = "" }, kind: FailureInvalidRequest},
		{name: "nested filename", mutate: func(r *DownloadRequest) { r.FileName = `nested\uv.zip` }, kind: FailureInvalidRequest},
		{name: "zero size", mutate: func(r *DownloadRequest) { r.ExpectedSize = 0 }, kind: FailureInvalidRequest},
		{name: "negative size", mutate: func(r *DownloadRequest) { r.ExpectedSize = -1 }, kind: FailureInvalidRequest},
		{name: "empty sha", mutate: func(r *DownloadRequest) { r.ExpectedSHA256 = "" }, kind: FailureInvalidRequest},
		{name: "short sha", mutate: func(r *DownloadRequest) { r.ExpectedSHA256 = "ab" }, kind: FailureInvalidRequest},
		{name: "non hex sha", mutate: func(r *DownloadRequest) { r.ExpectedSHA256 = strings.Repeat("z", 64) }, kind: FailureInvalidRequest},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sessions := &fakeSessionFactory{}
			client := &fakeHTTPClient{}
			clockCalls := 0
			timerCalls := 0
			cleanupCalls := 0
			progressCalls := 0
			options, err := resolveDownloaderOptions(nil)
			if err != nil {
				t.Fatalf("resolveDownloaderOptions() error = %v, want nil", err)
			}
			dependencies := testDependencies(sessions, client)
			dependencies.clock = func() time.Time {
				clockCalls++
				return time.Unix(1, 0)
			}
			dependencies.timers = func(time.Duration) timer {
				timerCalls++
				return newInertTimer()
			}
			dependencies.cleanup = func(ctx context.Context) (context.Context, context.CancelFunc) {
				cleanupCalls++
				return context.WithCancel(context.WithoutCancel(ctx))
			}
			downloader, err := newDownloaderWithDependencies(
				testLayout(t),
				options,
				dependencies,
			)
			if err != nil {
				t.Fatalf("newDownloaderWithDependencies() error = %v, want nil", err)
			}
			request := validRequest()
			request.Progress = func(DownloadProgress) error {
				progressCalls++
				return nil
			}
			if testCase.mutate != nil {
				testCase.mutate(&request)
			}
			var ctx context.Context = t.Context()
			if testCase.name == "nil context" {
				ctx = nil
			}
			_, failure := downloader.validateRequest(ctx, request)
			if failure == nil {
				t.Fatal("validateRequest() failure = nil, want non-nil")
			}
			if failure.Kind != testCase.kind {
				t.Fatalf("validateRequest() kind = %q, want %q", failure.Kind, testCase.kind)
			}
			if sessions.Calls() != 0 || client.Calls() != 0 {
				t.Fatalf("side effects = Begin:%d Do:%d, want 0/0", sessions.Calls(), client.Calls())
			}
			if clockCalls != 0 || timerCalls != 0 || cleanupCalls != 0 || progressCalls != 0 {
				t.Fatalf(
					"secondary side effects = clock:%d timer:%d cleanup:%d progress:%d, want 0/0/0/0",
					clockCalls,
					timerCalls,
					cleanupCalls,
					progressCalls,
				)
			}
		})
	}
}

func TestDownloadFailure_SanitizesTextAndPreservesCauses(t *testing.T) {
	mainCause := &testTypedError{cause: errTestSecret}
	cleanupCause := errors.New("cleanup-secret#fragment")
	failure := &DownloadFailure{
		Kind:       FailureHTTPStatus,
		StatusCode: http.StatusUnauthorized,
		Err:        safeExternalError("http request failed", mainCause),
		CleanupErr: safeExternalError("download cleanup failed", cleanupCause),
	}
	text := failure.Error() + " " + failure.Err.Error() + " " + failure.CleanupErr.Error()
	for _, secret := range []string{"callback-secret", "token=raw", "cleanup-secret", "fragment"} {
		if strings.Contains(text, secret) {
			t.Fatalf("public error text %q contains secret %q", text, secret)
		}
	}
	if !errors.Is(failure, errTestSecret) {
		t.Fatal("errors.Is(failure, errTestSecret) = false, want true")
	}
	if !errors.Is(failure, cleanupCause) {
		t.Fatal("errors.Is(failure, cleanupCause) = false, want true")
	}
	var typed *testTypedError
	if !errors.As(failure, &typed) {
		t.Fatal("errors.As(failure, *testTypedError) = false, want true")
	}
	if got := failure.Unwrap(); len(got) != 2 {
		t.Fatalf("len(failure.Unwrap()) = %d, want 2", len(got))
	}
}

type testTypedError struct {
	cause error
}

func (e *testTypedError) Error() string {
	return "typed-secret-error"
}

func (e *testTypedError) Unwrap() error {
	return e.cause
}

var _ io.Writer = (*fakeDownloadSession)(nil)

type bodyRead struct {
	data []byte
	err  error
}

type scriptedBody struct {
	ctx        context.Context
	reads      chan bodyRead
	secondRead chan struct{}
	readCount  atomic.Int32
	closeCount atomic.Int32
}

func newScriptedBody(ctx context.Context, reads ...bodyRead) *scriptedBody {
	channel := make(chan bodyRead, len(reads))
	for _, read := range reads {
		channel <- read
	}
	return &scriptedBody{
		ctx:        ctx,
		reads:      channel,
		secondRead: make(chan struct{}),
	}
}

func (b *scriptedBody) Read(p []byte) (int, error) {
	select {
	case read := <-b.reads:
		count := b.readCount.Add(1)
		if count == 2 {
			close(b.secondRead)
		}
		n := copy(p, read.data)
		return n, read.err
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *scriptedBody) Close() error {
	b.closeCount.Add(1)
	return nil
}

type progressClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *progressClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.times) == 0 {
		return time.Unix(100, 0)
	}
	value := c.times[0]
	c.times = c.times[1:]
	return value
}

func TestDownloader_ReadPumpOwnsChunkBackingArray(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := newScriptedBody(
		ctx,
		bodyRead{data: []byte("abc")},
		bodyRead{data: []byte("xyz")},
		bodyRead{err: io.EOF},
	)
	firstWrite := make(chan struct{})
	releaseWrite := make(chan struct{})
	writes := make([]string, 0, 2)
	session := &fakeDownloadSession{write: func(p []byte) (int, error) {
		if len(writes) == 0 {
			close(firstWrite)
			waitSignal(t, body.secondRead)
			if got := string(p); got != "abc" {
				t.Fatalf("first chunk changed to %q, want abc", got)
			}
			close(releaseWrite)
		}
		writes = append(writes, string(p))
		return len(p), nil
	}}
	downloader := downloaderForTransferTest(
		t,
		func(time.Duration) timer { return newManualTimer() },
		func() time.Time { return time.Unix(1, 0) },
	)
	request := validatedForBytes(t, []byte("abcxyz"), nil)
	response := &http.Response{Body: body, ContentLength: 6}
	received, digest, failure := downloader.readResponse(
		t.Context(),
		ctx,
		cancel,
		response,
		session,
		request,
		newProgressReporter(nil, request.expectedSize, time.Second, downloader.clock),
	)
	waitSignal(t, firstWrite)
	waitSignal(t, releaseWrite)
	if failure != nil {
		t.Fatalf("readResponse() failure = %v, want nil", failure)
	}
	if received != 6 || strings.Join(writes, "") != "abcxyz" {
		t.Fatalf("received/writes = %d/%q, want 6/abcxyz", received, strings.Join(writes, ""))
	}
	if digest != sha256.Sum256([]byte("abcxyz")) {
		t.Fatalf("digest = %x, want %x", digest, sha256.Sum256([]byte("abcxyz")))
	}
}

func TestDownloader_ReadIdleTimerExcludesBlockingWrite(t *testing.T) {
	runReadIdleLocalWorkTest(t, "write")
}

func TestDownloader_ReadIdleTimerExcludesBlockingProgress(t *testing.T) {
	runReadIdleLocalWorkTest(t, "progress")
}

func runReadIdleLocalWorkTest(t *testing.T, phase string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := newScriptedBody(ctx, bodyRead{data: []byte("abc")})
	readTimer := newManualTimer()
	localStarted := make(chan struct{})
	releaseLocal := make(chan struct{})
	session := &fakeDownloadSession{}
	var progress ProgressFunc
	if phase == "write" {
		session.write = func(p []byte) (int, error) {
			if !readTimer.stopped.Load() {
				t.Fatal("read timer is running during local Write")
			}
			close(localStarted)
			<-releaseLocal
			return len(p), nil
		}
	} else {
		progress = func(value DownloadProgress) error {
			if value.Received == 0 {
				return nil
			}
			if !readTimer.stopped.Load() {
				t.Fatal("read timer is running during local Progress")
			}
			close(localStarted)
			<-releaseLocal
			return nil
		}
	}
	downloader := downloaderForTransferTest(
		t,
		func(time.Duration) timer { return readTimer },
		func() time.Time { return time.Unix(1, 0) },
	)
	request := validatedForBytes(t, []byte("abc"), progress)
	result := make(chan *DownloadFailure, 1)
	go func() {
		_, _, failure := downloader.readResponse(
			ctx,
			ctx,
			cancel,
			&http.Response{Body: body, ContentLength: 3},
			session,
			request,
			newProgressReporter(
				progress,
				3,
				time.Nanosecond,
				downloader.clock,
			),
		)
		result <- failure
	}()

	waitSignal(t, localStarted)
	if readTimer.Fire() {
		t.Fatal("read timer fired while local work exceeded its timeout")
	}
	select {
	case failure := <-result:
		t.Fatalf("readResponse() returned during local work: %v", failure)
	default:
	}
	select {
	case <-readTimer.reset:
		t.Fatal("read timer reset before local work was released")
	default:
	}

	close(releaseLocal)
	waitSignal(t, readTimer.reset)
	if !readTimer.Fire() {
		t.Fatal("read timer did not fire during the next network wait")
	}
	failure := waitValue(t, result)
	if failure == nil || failure.Kind != FailureReadTimeout {
		t.Fatalf("failure = %#v, want FailureReadTimeout after next wait", failure)
	}
	if body.closeCount.Load() != 1 {
		t.Fatalf("Body.Close count = %d, want 1", body.closeCount.Load())
	}
}

func TestDownloader_ReadTimeoutAndCancellationJoinPump(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(context.CancelFunc, *manualTimer)
		kind    FailureKind
	}{
		{
			name: "read timeout",
			trigger: func(_ context.CancelFunc, timer *manualTimer) {
				timer.Fire()
			},
			kind: FailureReadTimeout,
		},
		{
			name: "cancel",
			trigger: func(cancel context.CancelFunc, _ *manualTimer) {
				cancel()
			},
			kind: FailureCancelled,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := newScriptedBody(ctx)
			readTimer := newManualTimer()
			downloader := downloaderForTransferTest(
				t,
				func(time.Duration) timer { return readTimer },
				func() time.Time { return time.Unix(1, 0) },
			)
			request := validatedForBytes(t, []byte("abc"), nil)
			result := make(chan *DownloadFailure, 1)
			go func() {
				_, _, failure := downloader.readResponse(
					ctx,
					ctx,
					cancel,
					&http.Response{Body: body, ContentLength: -1},
					&fakeDownloadSession{},
					request,
					newProgressReporter(nil, 3, time.Second, downloader.clock),
				)
				result <- failure
			}()
			testCase.trigger(cancel, readTimer)
			failure := waitValue(t, result)
			if failure == nil || failure.Kind != testCase.kind {
				t.Fatalf("failure = %#v, want %q", failure, testCase.kind)
			}
			if body.closeCount.Load() != 1 {
				t.Fatalf("Body.Close count = %d, want 1", body.closeCount.Load())
			}
		})
	}
}

func TestDownloader_CancellationDuringLocalWorkWinsOverTerminalChunk(t *testing.T) {
	cases := []struct {
		name  string
		phase string
	}{
		{name: "write", phase: "write"},
		{name: "progress", phase: "progress"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := newScriptedBody(
				ctx,
				bodyRead{data: []byte("abc"), err: io.EOF},
			)
			localStarted := make(chan struct{})
			releaseLocal := make(chan struct{})
			session := &fakeDownloadSession{}
			var progress ProgressFunc
			if testCase.phase == "write" {
				session.write = func(p []byte) (int, error) {
					close(localStarted)
					<-releaseLocal
					return len(p), nil
				}
			} else {
				progress = func(value DownloadProgress) error {
					if value.Received == 0 {
						return nil
					}
					close(localStarted)
					<-releaseLocal
					return nil
				}
			}
			downloader := downloaderForTransferTest(
				t,
				func(time.Duration) timer { return newManualTimer() },
				func() time.Time { return time.Unix(1, 0) },
			)
			request := validatedForBytes(t, []byte("abc"), progress)
			result := make(chan *DownloadFailure, 1)
			go func() {
				_, _, failure := downloader.readResponse(
					ctx,
					ctx,
					cancel,
					&http.Response{Body: body, ContentLength: 3},
					session,
					request,
					newProgressReporter(
						progress,
						3,
						time.Nanosecond,
						downloader.clock,
					),
				)
				result <- failure
			}()

			waitSignal(t, localStarted)
			cancel()
			close(releaseLocal)
			failure := waitValue(t, result)
			if failure == nil || failure.Kind != FailureCancelled {
				t.Fatalf("failure = %#v, want FailureCancelled", failure)
			}
			if !errors.Is(failure, context.Canceled) {
				t.Fatal("failure does not preserve context.Canceled")
			}
			if body.closeCount.Load() != 1 {
				t.Fatalf("Body.Close count = %d, want 1", body.closeCount.Load())
			}
		})
	}
}

func TestDownloader_ProcessesBytesBeforeTerminalReadError(t *testing.T) {
	terminalErr := errors.New("terminal read error")
	cases := []struct {
		name     string
		readErr  error
		wantKind FailureKind
	}{
		{name: "bytes and EOF", readErr: io.EOF},
		{name: "bytes and error", readErr: terminalErr, wantKind: FailureNetwork},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := newScriptedBody(ctx, bodyRead{data: []byte("abc"), err: testCase.readErr})
			written := make([]byte, 0, 3)
			session := &fakeDownloadSession{write: func(p []byte) (int, error) {
				written = append(written, p...)
				return len(p), nil
			}}
			downloader := downloaderForTransferTest(
				t,
				func(time.Duration) timer { return newManualTimer() },
				func() time.Time { return time.Unix(1, 0) },
			)
			request := validatedForBytes(t, []byte("abc"), nil)
			received, digest, failure := downloader.readResponse(
				t.Context(),
				ctx,
				cancel,
				&http.Response{Body: body, ContentLength: 3},
				session,
				request,
				newProgressReporter(nil, 3, time.Second, downloader.clock),
			)
			if received != 3 || string(written) != "abc" {
				t.Fatalf("received/written = %d/%q, want 3/abc", received, written)
			}
			if digest != sha256.Sum256([]byte("abc")) {
				t.Fatalf("digest = %x, want abc digest", digest)
			}
			if testCase.wantKind == "" {
				if failure != nil {
					t.Fatalf("failure = %v, want nil", failure)
				}
				return
			}
			if failure == nil || failure.Kind != testCase.wantKind {
				t.Fatalf("failure = %#v, want %q", failure, testCase.wantKind)
			}
			if !errors.Is(failure, terminalErr) {
				t.Fatal("terminal read cause was not preserved")
			}
		})
	}
}

func TestDownloader_ShortWriteIsNotRetriedOrCounted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := newScriptedBody(ctx, bodyRead{data: []byte("abc")}, bodyRead{err: io.EOF})
	var writeCalls atomic.Int32
	session := &fakeDownloadSession{write: func([]byte) (int, error) {
		writeCalls.Add(1)
		return 1, nil
	}}
	downloader := downloaderForTransferTest(
		t,
		func(time.Duration) timer { return newManualTimer() },
		func() time.Time { return time.Unix(1, 0) },
	)
	request := validatedForBytes(t, []byte("abc"), nil)
	received, _, failure := downloader.readResponse(
		t.Context(),
		ctx,
		cancel,
		&http.Response{Body: body, ContentLength: 3},
		session,
		request,
		newProgressReporter(nil, 3, time.Second, downloader.clock),
	)
	if failure == nil || failure.Kind != FailureFilesystem {
		t.Fatalf("failure = %#v, want FailureFilesystem", failure)
	}
	if !errors.Is(failure, io.ErrShortWrite) {
		t.Fatal("failure does not preserve io.ErrShortWrite")
	}
	if writeCalls.Load() != 1 || received != 0 {
		t.Fatalf("write calls/received = %d/%d, want 1/0", writeCalls.Load(), received)
	}
}

func TestDownloader_SizeAccountingDoesNotOverflow(t *testing.T) {
	cases := []struct {
		name     string
		received int64
		written  int
		expected int64
		want     int64
		wantErr  bool
	}{
		{name: "exact MaxInt64", received: math.MaxInt64 - 3, written: 3, expected: math.MaxInt64, want: math.MaxInt64},
		{name: "overflow target", received: math.MaxInt64 - 2, written: 3, expected: math.MaxInt64, want: math.MaxInt64 - 2, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := advanceReceived(testCase.received, testCase.written, testCase.expected)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("advanceReceived() error = %v, wantErr %t", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Fatalf("advanceReceived() = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestDownloader_ProgressInitialMonotonicFinalAndCallbackSafety(t *testing.T) {
	clock := &progressClock{times: []time.Time{
		time.Unix(1, 0),
		time.Unix(1, 100),
		time.Unix(2, 0),
	}}
	var values []DownloadProgress
	reporter := newProgressReporter(
		func(progress DownloadProgress) error {
			values = append(values, progress)
			return nil
		},
		3,
		time.Second,
		clock.Now,
	)
	for _, step := range []struct {
		received int64
		force    bool
	}{
		{received: 0, force: true},
		{received: 1},
		{received: 3, force: true},
	} {
		if err := reporter.report(step.received, step.force); err != nil {
			t.Fatalf("report() error = %v, want nil", err)
		}
	}
	if len(values) != 2 {
		t.Fatalf("progress count = %d, want 2", len(values))
	}
	if values[0].Received != 0 || values[1].Received != 3 ||
		values[1].Total != 3 || values[1].Percent != 100 {
		t.Fatalf("progress values = %#v, want initial 0 and final 3/100", values)
	}

	reporter = newProgressReporter(
		func(DownloadProgress) error { return errTestSecret },
		3,
		time.Second,
		clock.Now,
	)
	err := reporter.report(0, true)
	if err == nil || strings.Contains(err.Error(), "callback-secret") {
		t.Fatalf("progress error = %v, want sanitized non-nil", err)
	}
	if !errors.Is(err, errTestSecret) {
		t.Fatal("progress error does not preserve callback cause")
	}
}

func downloaderForTransferTest(
	t *testing.T,
	timers timerFactory,
	clock func() time.Time,
) *Downloader {
	t.Helper()
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v, want nil", err)
	}
	dependencies := testDependencies(&fakeSessionFactory{}, &fakeHTTPClient{})
	dependencies.timers = timers
	dependencies.clock = clock
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

func validatedForBytes(
	t *testing.T,
	content []byte,
	progress ProgressFunc,
) validatedRequest {
	t.Helper()
	digest := sha256.Sum256(content)
	return validatedRequest{
		expectedSize:   int64(len(content)),
		expectedSHA256: hex.EncodeToString(digest[:]),
		expectedDigest: digest,
		progress:       progress,
	}
}
