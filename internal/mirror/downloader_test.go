package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

type recordingSession struct {
	fakeDownloadSession
	mu            sync.Mutex
	content       []byte
	events        []string
	writeResult   int
	writeErr      error
	writeFault    bool
	publishResult filesystem.PublishResult
	publishErr    error
	abortResult   filesystem.AbortResult
	abortErr      error
	publish       func(context.Context) (filesystem.PublishResult, error)
	abort         func(context.Context) (filesystem.AbortResult, error)
	writeHook     func()
}

func (s *recordingSession) Write(p []byte) (int, error) {
	if s.writeHook != nil {
		s.writeHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "write")
	if s.writeFault {
		return s.writeResult, s.writeErr
	}
	s.content = append(s.content, p...)
	return len(p), nil
}

func (s *recordingSession) PublishNoReplace(
	ctx context.Context,
) (filesystem.PublishResult, error) {
	s.mu.Lock()
	s.events = append(s.events, "publish")
	publish := s.publish
	result := s.publishResult
	err := s.publishErr
	s.mu.Unlock()
	if publish != nil {
		return publish(ctx)
	}
	return result, err
}

func (s *recordingSession) Abort(
	ctx context.Context,
) (filesystem.AbortResult, error) {
	s.mu.Lock()
	s.abortCalls++
	s.events = append(s.events, "abort")
	abortErr := s.abortErr
	abortResult := s.abortResult
	abort := s.abort
	s.mu.Unlock()
	if abort != nil {
		return abort(ctx)
	}
	if abortErr != nil {
		return abortResult, abortErr
	}
	select {
	case <-ctx.Done():
		return abortResult, ctx.Err()
	default:
		return abortResult, nil
	}
}

func (s *recordingSession) snapshot() ([]byte, []string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.content...),
		append([]string(nil), s.events...),
		s.abortCalls
}

func TestDownloader_DownloadPublishesVerifiedResult(t *testing.T) {
	content := []byte("verified-content")
	session := &recordingSession{
		fakeDownloadSession: fakeDownloadSession{
			path:     `C:\app\runtime\cache\downloads\uv.zip`,
			partPath: `C:\app\runtime\cache\downloads\uv.zip.part`,
		},
		publishResult: filesystem.PublishResult{Published: true},
		abortResult:   filesystem.AbortResult{Removed: true},
	}
	client := responseClient(t, content, int64(len(content)), http.StatusOK)
	downloader := downloaderForTransactionTest(t, session, client, nil)
	request := requestForBytes(content)
	var progress []DownloadProgress
	request.Progress = func(value DownloadProgress) error {
		progress = append(progress, value)
		return nil
	}
	result, err := downloader.Download(t.Context(), request)
	if err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	digest := sha256.Sum256(content)
	if result.Path != session.path ||
		result.Size != int64(len(content)) ||
		result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("Download() result = %#v, want verified path/size/digest", result)
	}
	written, events, abortCalls := session.snapshot()
	if !bytes.Equal(written, content) {
		t.Fatalf("written bytes = %q, want %q", written, content)
	}
	if got := strings.Join(events, ","); got != "write,publish" {
		t.Fatalf("events = %q, want write,publish", got)
	}
	if abortCalls != 0 {
		t.Fatalf("Abort calls = %d, want 0", abortCalls)
	}
	if len(progress) < 2 ||
		progress[0].Received != 0 ||
		progress[len(progress)-1].Received != int64(len(content)) {
		t.Fatalf("progress = %#v, want initial and final", progress)
	}
}

func TestDownloader_DownloadValidatesBeforeBeginOrNetwork(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DownloadRequest)
	}{
		{name: "URL", mutate: func(request *DownloadRequest) { request.URL = "http://127.0.0.1/asset" }},
		{name: "file", mutate: func(request *DownloadRequest) { request.FileName = `nested\asset` }},
		{name: "size", mutate: func(request *DownloadRequest) { request.ExpectedSize = 0 }},
		{name: "checksum", mutate: func(request *DownloadRequest) { request.ExpectedSHA256 = "invalid" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sessions := &fakeSessionFactory{}
			client := &fakeHTTPClient{}
			options, err := resolveDownloaderOptions(nil)
			if err != nil {
				t.Fatalf("resolveDownloaderOptions() error = %v", err)
			}
			downloader, err := newDownloaderWithDependencies(
				testLayout(t),
				options,
				testDependencies(sessions, client),
			)
			if err != nil {
				t.Fatalf("newDownloaderWithDependencies() error = %v", err)
			}
			request := requestForBytes([]byte("abc"))
			testCase.mutate(&request)
			_, err = downloader.Download(t.Context(), request)
			var failure *DownloadFailure
			if !errors.As(err, &failure) {
				t.Fatalf("Download() error = %v, want *DownloadFailure", err)
			}
			if sessions.Calls() != 0 || client.Calls() != 0 {
				t.Fatalf("side effects = Begin:%d Do:%d, want 0/0", sessions.Calls(), client.Calls())
			}
		})
	}
}

func TestDownloader_DownloadAcceptsMissingContentLength(t *testing.T) {
	content := []byte("abc")
	session := successfulRecordingSession()
	client := responseClient(t, content, -1, http.StatusOK)
	downloader := downloaderForTransactionTest(t, session, client, nil)
	result, err := downloader.Download(t.Context(), requestForBytes(content))
	if err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	if result.Size != 3 {
		t.Fatalf("result.Size = %d, want 3", result.Size)
	}
}

func TestDownloader_UnpublishedFailuresAbortAndPreserveMainKind(t *testing.T) {
	content := []byte("abc")
	progressErr := errors.New("progress-private-message")
	readErr := errors.New("read-private-message")
	cases := []struct {
		name       string
		request    DownloadRequest
		client     httpClient
		session    *recordingSession
		wantKind   FailureKind
		wantStatus int
		wantCause  error
	}{
		{
			name:       "HTTP status",
			request:    requestForBytes(content),
			client:     responseClient(t, []byte("private body"), 12, http.StatusNotFound),
			session:    successfulRecordingSession(),
			wantKind:   FailureHTTPStatus,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "HTTP 429",
			request:    requestForBytes(content),
			client:     responseClient(t, []byte("private body"), 12, http.StatusTooManyRequests),
			session:    successfulRecordingSession(),
			wantKind:   FailureHTTPStatus,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "HTTP 500",
			request:    requestForBytes(content),
			client:     responseClient(t, []byte("private body"), 12, http.StatusInternalServerError),
			session:    successfulRecordingSession(),
			wantKind:   FailureHTTPStatus,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:     "Content-Length mismatch",
			request:  requestForBytes(content),
			client:   responseClient(t, content, 4, http.StatusOK),
			session:  successfulRecordingSession(),
			wantKind: FailureSizeMismatch,
		},
		{
			name: "short body",
			request: DownloadRequest{
				URL:            validRequest().URL,
				FileName:       "uv.zip",
				ExpectedSize:   4,
				ExpectedSHA256: strings.Repeat("a", 64),
			},
			client:   responseClient(t, content, -1, http.StatusOK),
			session:  successfulRecordingSession(),
			wantKind: FailureSizeMismatch,
		},
		{
			name:     "long body",
			request:  requestForBytes(content),
			client:   responseClient(t, []byte("abcd"), -1, http.StatusOK),
			session:  successfulRecordingSession(),
			wantKind: FailureSizeMismatch,
		},
		{
			name:     "checksum mismatch",
			request:  validRequest(),
			client:   responseClient(t, content, 3, http.StatusOK),
			session:  successfulRecordingSession(),
			wantKind: FailureChecksumMismatch,
		},
		{
			name: "progress",
			request: func() DownloadRequest {
				request := requestForBytes(content)
				request.Progress = func(DownloadProgress) error { return progressErr }
				return request
			}(),
			client:    responseClient(t, content, 3, http.StatusOK),
			session:   successfulRecordingSession(),
			wantKind:  FailureProgress,
			wantCause: progressErr,
		},
		{
			name:    "body read",
			request: requestForBytes(content),
			client: responseWithBodyClient(t, &terminalBody{
				content: content,
				err:     readErr,
			}, 3, http.StatusOK),
			session:   successfulRecordingSession(),
			wantKind:  FailureNetwork,
			wantCause: readErr,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			downloader := downloaderForTransactionTest(
				t,
				testCase.session,
				testCase.client,
				nil,
			)
			result, err := downloader.Download(t.Context(), testCase.request)
			var failure *DownloadFailure
			if !errors.As(err, &failure) {
				t.Fatalf("Download() error = %v, want *DownloadFailure", err)
			}
			if failure.Kind != testCase.wantKind ||
				failure.StatusCode != testCase.wantStatus ||
				failure.Published {
				t.Fatalf("failure = %#v, want kind/status unpublished", failure)
			}
			if result != (DownloadResult{}) {
				t.Fatalf("result = %#v, want zero", result)
			}
			_, _, abortCalls := testCase.session.snapshot()
			if abortCalls != 1 {
				t.Fatalf("Abort calls = %d, want 1", abortCalls)
			}
			if testCase.wantCause != nil && !errors.Is(failure, testCase.wantCause) {
				t.Fatalf("failure does not preserve cause %v", testCase.wantCause)
			}
			if strings.Contains(
				failure.Error()+" "+errorText(failure.Err),
				"private",
			) {
				t.Fatalf("public failure text leaked external text: %v", failure)
			}
		})
	}
}

func TestDownloader_DownloadFaultMatrix(t *testing.T) {
	content := []byte("abc")
	beginCause := errors.New("begin-secret")
	beginTyped := &testTypedError{cause: beginCause}
	beginSessions := &fakeSessionFactory{err: beginTyped}
	beginClient := &fakeHTTPClient{}
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v", err)
	}
	beginDownloader, err := newDownloaderWithDependencies(
		testLayout(t),
		options,
		testDependencies(beginSessions, beginClient),
	)
	if err != nil {
		t.Fatalf("newDownloaderWithDependencies() error = %v", err)
	}
	beginResult, beginErr := beginDownloader.Download(
		t.Context(),
		requestForBytes(content),
	)
	var beginFailure *DownloadFailure
	var beginGotTyped *testTypedError
	if beginResult != (DownloadResult{}) ||
		!errors.As(beginErr, &beginFailure) ||
		beginFailure.Kind != FailureFilesystem ||
		beginFailure.Published ||
		!errors.Is(beginErr, beginCause) ||
		!errors.As(beginErr, &beginGotTyped) ||
		beginGotTyped != beginTyped ||
		beginClient.Calls() != 0 {
		t.Fatalf(
			"Begin fault result/error/client calls = %#v/%v/%d",
			beginResult,
			beginErr,
			beginClient.Calls(),
		)
	}
	if strings.Contains(
		beginFailure.Error()+" "+errorText(beginFailure.Err),
		"secret",
	) {
		t.Fatalf("Begin public failure leaked cause: %v", beginFailure)
	}

	mainCause := errors.New("main-secret")
	mainTyped := &testTypedError{cause: mainCause}
	cleanupCause := errors.New("cleanup-secret")
	cleanupTyped := &testTypedError{cause: cleanupCause}
	cases := []struct {
		name        string
		configure   func(*recordingSession, *closeCountingBody)
		bodyReader  func() io.Reader
		wantKind    FailureKind
		wantMain    error
		wantCleanup error
	}{
		{
			name: "write",
			configure: func(session *recordingSession, _ *closeCountingBody) {
				session.writeFault = true
				session.writeErr = mainTyped
			},
			wantKind: FailureFilesystem,
			wantMain: mainCause,
		},
		{
			name: "short write",
			configure: func(session *recordingSession, _ *closeCountingBody) {
				session.writeFault = true
				session.writeResult = 1
			},
			wantKind: FailureFilesystem,
			wantMain: io.ErrShortWrite,
		},
		{
			name: "Flush in publish phase",
			configure: func(session *recordingSession, _ *closeCountingBody) {
				session.publishResult = filesystem.PublishResult{}
				session.publishErr = mainTyped
			},
			wantKind: FailureFilesystem,
			wantMain: mainCause,
		},
		{
			name: "Body Close",
			configure: func(_ *recordingSession, body *closeCountingBody) {
				body.err = mainTyped
			},
			wantKind: FailureNetwork,
			wantMain: mainCause,
		},
		{
			name: "Publish",
			configure: func(session *recordingSession, _ *closeCountingBody) {
				session.publishResult = filesystem.PublishResult{}
				session.publishErr = filesystem.ErrDestinationExists
			},
			wantKind: FailureDestinationOccupied,
			wantMain: filesystem.ErrDestinationExists,
		},
		{
			name: "Abort",
			configure: func(session *recordingSession, _ *closeCountingBody) {
				session.abortErr = cleanupTyped
			},
			bodyReader: func() io.Reader {
				return &terminalBody{content: content, err: mainTyped}
			},
			wantKind:    FailureNetwork,
			wantMain:    mainCause,
			wantCleanup: cleanupCause,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			session := successfulRecordingSession()
			reader := io.Reader(bytes.NewReader(content))
			if testCase.bodyReader != nil {
				reader = testCase.bodyReader()
			}
			body := &closeCountingBody{reader: reader}
			testCase.configure(session, body)
			downloader := downloaderForTransactionTest(
				t,
				session,
				responseWithBodyClient(
					t,
					body,
					int64(len(content)),
					http.StatusOK,
				),
				nil,
			)
			result, err := downloader.Download(
				t.Context(),
				requestForBytes(content),
			)
			var failure *DownloadFailure
			if result != (DownloadResult{}) ||
				!errors.As(err, &failure) ||
				failure.Kind != testCase.wantKind ||
				failure.Published ||
				!errors.Is(err, testCase.wantMain) {
				t.Fatalf(
					"Download() result/error = %#v/%v, want %q unpublished with main cause",
					result,
					err,
					testCase.wantKind,
				)
			}
			if testCase.wantMain == mainCause {
				var gotTyped *testTypedError
				if !errors.As(err, &gotTyped) || gotTyped != mainTyped {
					t.Fatalf("main errors.As() = %#v, want original", gotTyped)
				}
			}
			if testCase.wantCleanup != nil {
				if failure.CleanupErr == nil || !errors.Is(err, testCase.wantCleanup) {
					t.Fatalf("cleanup chain = %#v, want cleanup cause", failure)
				}
			} else if failure.CleanupErr != nil {
				t.Fatalf("CleanupErr = %v, want nil", failure.CleanupErr)
			}
			_, _, abortCalls := session.snapshot()
			if abortCalls != 1 || body.count.Load() != 1 {
				t.Fatalf(
					"Abort/Body.Close calls = %d/%d, want 1/1",
					abortCalls,
					body.count.Load(),
				)
			}
			public := failure.Error() + " " +
				errorText(failure.Err) + " " +
				errorText(failure.CleanupErr)
			if strings.Contains(public, "secret") {
				t.Fatalf("public failure text leaked cause: %q", public)
			}
		})
	}
}

func TestDownloader_PublishResultControlsAbortAndNormalizesResult(t *testing.T) {
	publishErr := errors.New("publish failed")
	cases := []struct {
		name       string
		result     filesystem.PublishResult
		publishErr error
		wantKind   FailureKind
		wantResult bool
		wantAbort  int
	}{
		{
			name:       "destination occupied",
			publishErr: filesystem.ErrDestinationExists,
			wantKind:   FailureDestinationOccupied,
			wantAbort:  1,
		},
		{
			name:       "rename did not publish",
			publishErr: publishErr,
			wantKind:   FailureFilesystem,
			wantAbort:  1,
		},
		{
			name:       "rename published then close failed",
			result:     filesystem.PublishResult{Published: true},
			publishErr: publishErr,
			wantKind:   FailureFilesystem,
			wantResult: true,
			wantAbort:  0,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			session := successfulRecordingSession()
			session.publishResult = testCase.result
			session.publishErr = testCase.publishErr
			content := []byte("abc")
			downloader := downloaderForTransactionTest(
				t,
				session,
				responseClient(t, content, 3, http.StatusOK),
				nil,
			)
			request := requestForBytes(content)
			if testCase.result.Published {
				request.ExpectedSHA256 = strings.ToUpper(
					request.ExpectedSHA256,
				)
			}
			result, err := downloader.Download(t.Context(), request)
			var failure *DownloadFailure
			if !errors.As(err, &failure) {
				t.Fatalf("Download() error = %v, want *DownloadFailure", err)
			}
			if failure.Kind != testCase.wantKind ||
				failure.Published != testCase.result.Published {
				t.Fatalf("failure = %#v, want kind %q published %t", failure, testCase.wantKind, testCase.result.Published)
			}
			if (result != (DownloadResult{})) != testCase.wantResult {
				t.Fatalf("result = %#v, wantResult %t", result, testCase.wantResult)
			}
			if failure.Published {
				digest := sha256.Sum256(content)
				want := DownloadResult{
					Path:   session.path,
					Size:   int64(len(content)),
					SHA256: hex.EncodeToString(digest[:]),
				}
				if result != want {
					t.Fatalf(
						"published failure result = %#v, want %#v",
						result,
						want,
					)
				}
				if failure.CleanupErr != nil {
					t.Fatalf(
						"published failure CleanupErr = %v, want nil",
						failure.CleanupErr,
					)
				}
			}
			_, _, abortCalls := session.snapshot()
			if abortCalls != testCase.wantAbort {
				t.Fatalf("Abort calls = %d, want %d", abortCalls, testCase.wantAbort)
			}
		})
	}
}

type closeCountingBody struct {
	reader io.Reader
	count  atomic.Int32
	err    error
}

func (b *closeCountingBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *closeCountingBody) Close() error {
	b.count.Add(1)
	return b.err
}

func TestDownloader_BodyClosedExactlyOnceAcrossResponsePaths(t *testing.T) {
	content := []byte("abc")
	cases := []struct {
		name          string
		status        int
		contentLength int64
		request       DownloadRequest
	}{
		{name: "success", status: http.StatusOK, contentLength: 3, request: requestForBytes(content)},
		{name: "HTTP error", status: http.StatusNotFound, contentLength: 3, request: requestForBytes(content)},
		{name: "length mismatch", status: http.StatusOK, contentLength: 4, request: requestForBytes(content)},
		{name: "checksum mismatch", status: http.StatusOK, contentLength: 3, request: validRequest()},
		{
			name:          "progress error",
			status:        http.StatusOK,
			contentLength: 3,
			request: func() DownloadRequest {
				request := requestForBytes(content)
				request.Progress = func(progress DownloadProgress) error {
					if progress.Received == progress.Total {
						return errTestSecret
					}
					return nil
				}
				return request
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := &closeCountingBody{reader: bytes.NewReader(content)}
			client := responseWithBodyClient(
				t,
				body,
				testCase.contentLength,
				testCase.status,
			)
			downloader := downloaderForTransactionTest(
				t,
				successfulRecordingSession(),
				client,
				nil,
			)
			_, _ = downloader.Download(t.Context(), testCase.request)
			if body.count.Load() != 1 {
				t.Fatalf("Body.Close count = %d, want 1", body.count.Load())
			}
		})
	}
}

func TestDownloader_BodyCloseFailureMatrix(t *testing.T) {
	content := []byte("abc")
	closeCause := errors.New("close-cause-secret")
	typedClose := &testTypedError{cause: closeCause}
	cases := []struct {
		name          string
		status        int
		contentLength int64
		request       DownloadRequest
		wantKind      FailureKind
	}{
		{
			name:          "normal EOF",
			status:        http.StatusOK,
			contentLength: 3,
			request:       requestForBytes(content),
			wantKind:      FailureNetwork,
		},
		{
			name:          "non-200",
			status:        http.StatusNotFound,
			contentLength: 3,
			request:       requestForBytes(content),
			wantKind:      FailureHTTPStatus,
		},
		{
			name:          "Content-Length precheck",
			status:        http.StatusOK,
			contentLength: 4,
			request:       requestForBytes(content),
			wantKind:      FailureSizeMismatch,
		},
		{
			name:          "checksum primary precedes close",
			status:        http.StatusOK,
			contentLength: 3,
			request:       validRequest(),
			wantKind:      FailureChecksumMismatch,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := &closeCountingBody{
				reader: bytes.NewReader(content),
				err:    typedClose,
			}
			session := successfulRecordingSession()
			downloader := downloaderForTransactionTest(
				t,
				session,
				responseWithBodyClient(
					t,
					body,
					testCase.contentLength,
					testCase.status,
				),
				nil,
			)
			result, err := downloader.Download(t.Context(), testCase.request)
			var failure *DownloadFailure
			var gotTyped *testTypedError
			if !errors.As(err, &failure) ||
				failure.Kind != testCase.wantKind ||
				failure.Published ||
				!errors.Is(err, closeCause) ||
				!errors.As(err, &gotTyped) ||
				gotTyped != typedClose {
				t.Fatalf(
					"Download() result/error = %#v/%v, want %q unpublished with typed close cause",
					result,
					err,
					testCase.wantKind,
				)
			}
			if result != (DownloadResult{}) {
				t.Fatalf("Download() result = %#v, want zero", result)
			}
			_, _, abortCalls := session.snapshot()
			if body.count.Load() != 1 || abortCalls != 1 {
				t.Fatalf(
					"Close/Abort calls = %d/%d, want 1/1",
					body.count.Load(),
					abortCalls,
				)
			}
			public := failure.Error() + " " +
				errorText(failure.Err) + " " +
				errorText(failure.CleanupErr)
			if strings.Contains(public, "secret") {
				t.Fatalf("public failure text leaked cause: %q", public)
			}
		})
	}
}

func TestDownloader_PublishesOnlyAfterBodyCloseAndNeverCallsBackAfter(t *testing.T) {
	content := []byte("abc")
	body := &orderedBody{
		reader: strings.NewReader(string(content)),
		events: make(chan string, 4),
	}
	session := successfulRecordingSession()
	var published atomic.Bool
	callbackCalls := atomic.Int32{}
	finalCallback := make(chan struct{})
	releaseCallback := make(chan struct{})
	session.publish = func(context.Context) (filesystem.PublishResult, error) {
		if got := waitValue(t, body.events); got != "close" {
			t.Fatalf("event before publish = %q, want close", got)
		}
		published.Store(true)
		return filesystem.PublishResult{Published: true}, nil
	}
	request := requestForBytes(content)
	request.Progress = func(progress DownloadProgress) error {
		if published.Load() {
			return errors.New("progress called after publish")
		}
		callbackCalls.Add(1)
		if progress.Received == progress.Total {
			close(finalCallback)
			<-releaseCallback
		}
		return nil
	}
	downloader := downloaderForTransactionTest(
		t,
		session,
		responseWithBodyClient(t, body, 3, http.StatusOK),
		nil,
	)
	result := make(chan error, 1)
	go func() {
		_, err := downloader.Download(t.Context(), request)
		result <- err
	}()
	waitSignal(t, finalCallback)
	if published.Load() {
		t.Fatal("Publish occurred before the final progress callback returned")
	}
	close(releaseCallback)
	err := waitValue(t, result)
	if err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	if !published.Load() {
		t.Fatal("Publish was not observed")
	}
	if callbackCalls.Load() != 2 {
		t.Fatalf("callback count = %d, want initial and final only", callbackCalls.Load())
	}
}

type cleanupValueKey struct{}

type sessionFactoryFunc func(
	ctx context.Context,
	name string,
) (downloadSession, error)

func (f sessionFactoryFunc) Begin(
	ctx context.Context,
	name string,
) (downloadSession, error) {
	return f(ctx, name)
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDownloader_ConcurrentDownloadsHaveIndependentState(t *testing.T) {
	contents := map[string][]byte{
		"/one": []byte("first"),
		"/two": []byte("second"),
	}
	writesArrived := make(chan struct{}, len(contents))
	releaseWrites := make(chan struct{})
	var created sync.Map
	sessions := sessionFactoryFunc(func(
		_ context.Context,
		name string,
	) (downloadSession, error) {
		session := successfulRecordingSession()
		session.path = `C:\downloads\` + name
		session.partPath = session.path + ".part"
		session.writeHook = func() {
			writesArrived <- struct{}{}
			<-releaseWrites
		}
		created.Store(name, session)
		return session, nil
	})
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		content := contents[request.URL.Path]
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(content)),
			Body:          io.NopCloser(bytes.NewReader(content)),
			Request:       request,
		}, nil
	})
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v", err)
	}
	dependencies := testDependencies(sessions, client)
	downloader, err := newDownloaderWithDependencies(
		testLayout(t),
		options,
		dependencies,
	)
	if err != nil {
		t.Fatalf("newDownloaderWithDependencies() error = %v", err)
	}
	start := make(chan struct{})
	type concurrentResult struct {
		name    string
		content []byte
		result  DownloadResult
		err     error
	}
	results := make(chan concurrentResult, len(contents))
	for path, content := range contents {
		path := path
		content := append([]byte(nil), content...)
		go func() {
			<-start
			request := requestForBytes(content)
			request.URL = "https://example.invalid" + path
			name := strings.TrimPrefix(path, "/") + ".zip"
			request.FileName = name
			result, err := downloader.Download(t.Context(), request)
			results <- concurrentResult{
				name:    name,
				content: content,
				result:  result,
				err:     err,
			}
		}()
	}
	close(start)
	for range contents {
		waitSignal(t, writesArrived)
	}
	close(releaseWrites)
	for range contents {
		got := waitValue(t, results)
		if got.err != nil {
			t.Fatalf("concurrent Download(%q) error = %v, want nil", got.name, got.err)
		}
		digest := sha256.Sum256(got.content)
		wantResult := DownloadResult{
			Path:   `C:\downloads\` + got.name,
			Size:   int64(len(got.content)),
			SHA256: hex.EncodeToString(digest[:]),
		}
		if got.result != wantResult {
			t.Fatalf("concurrent Download(%q) result = %#v, want %#v", got.name, got.result, wantResult)
		}
		value, ok := created.Load(got.name)
		if !ok {
			t.Fatalf("session %q was not recorded", got.name)
		}
		session := value.(*recordingSession)
		written, events, abortCalls := session.snapshot()
		if !bytes.Equal(written, got.content) ||
			strings.Join(events, ",") != "write,publish" ||
			abortCalls != 0 {
			t.Fatalf(
				"session %q content/events/abort = %q/%q/%d, want %q/write,publish/0",
				got.name,
				written,
				strings.Join(events, ","),
				abortCalls,
				got.content,
			)
		}
	}
}

func TestDownloader_CleanupIgnoresOperationCancelAndPreservesValues(t *testing.T) {
	content := []byte("abc")
	session := successfulRecordingSession()
	value := "cleanup-value"
	abortObserved := make(chan struct{})
	session.abort = func(ctx context.Context) (filesystem.AbortResult, error) {
		if got := ctx.Value(cleanupValueKey{}); got != value {
			t.Errorf("cleanup context value = %v, want %q", got, value)
		}
		if ctx.Err() != nil {
			t.Errorf("cleanup context error = %v, want nil on entry", ctx.Err())
		}
		close(abortObserved)
		return filesystem.AbortResult{Removed: true}, nil
	}
	doStarted := make(chan struct{})
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		close(doStarted)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	downloader := downloaderForTransactionTest(t, session, client, nil)
	operationCtx, cancel := context.WithCancel(
		context.WithValue(t.Context(), cleanupValueKey{}, value),
	)
	result := make(chan error, 1)
	go func() {
		_, err := downloader.Download(operationCtx, requestForBytes(content))
		result <- err
	}()
	waitSignal(t, doStarted)
	cancel()
	err := waitValue(t, result)
	var failure *DownloadFailure
	if !errors.As(err, &failure) || failure.Kind != FailureCancelled {
		t.Fatalf("Download() error = %v, want FailureCancelled", err)
	}
	waitSignal(t, abortObserved)
}

func TestDownloader_CleanupErrorDoesNotReplaceMainFailure(t *testing.T) {
	mainErr := errors.New("network-main-secret")
	cleanupErr := errors.New("cleanup-secondary-secret")
	session := successfulRecordingSession()
	session.abortResult = filesystem.AbortResult{Removed: false}
	session.abortErr = cleanupErr
	client := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return nil, mainErr
	}}
	downloader := downloaderForTransactionTest(t, session, client, nil)
	_, err := downloader.Download(t.Context(), requestForBytes([]byte("abc")))
	var failure *DownloadFailure
	if !errors.As(err, &failure) || failure.Kind != FailureNetwork {
		t.Fatalf("Download() error = %v, want FailureNetwork", err)
	}
	if failure.CleanupErr == nil ||
		!errors.Is(failure, mainErr) ||
		!errors.Is(failure, cleanupErr) {
		t.Fatalf("failure chains = %#v, want main and cleanup", failure)
	}
	text := failure.Error() + " " + errorText(failure.Err) + " " + errorText(failure.CleanupErr)
	if strings.Contains(text, "secret") {
		t.Fatalf("public error text leaked external message: %q", text)
	}
}

func TestDownloader_RotatorAttemptReportRedactsDownloadFailure(
	t *testing.T,
) {
	typedCause := &testTypedError{cause: errTestSecret}
	client := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{
			Op: "Get",
			URL: "https://user:password@example.invalid/" +
				"asset?token=url-secret#fragment",
			Err: typedCause,
		}
	}}
	downloader := downloaderForTransactionTest(
		t,
		successfulRecordingSession(),
		client,
		nil,
	)
	_, downloadErr := downloader.Download(
		t.Context(),
		requestForBytes([]byte("abc")),
	)
	var downloadFailure *DownloadFailure
	var leakedURL *url.Error
	if !errors.As(downloadErr, &downloadFailure) ||
		downloadFailure.Kind != FailureNetwork ||
		!errors.Is(downloadErr, errTestSecret) ||
		errors.As(downloadErr, &leakedURL) {
		t.Fatalf(
			"Downloader error = %v, want sanitized network failure preserving typed cause",
			downloadErr,
		)
	}

	rotator, err := NewRotator(WithMaxSourceAttempts(1))
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	policy, err := NewPolicy(PolicySpec{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := BuildPlan(catalog, policy, KindUV)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	target, err := NewTarget(TargetSpec{
		ProductVersion: "v5.3.0",
		ReleaseBranch:  "release/v5.3.0",
		UVVersion:      "0.8.12",
		PythonVersion:  "3.12.10",
		LockDigest:     strings.Repeat("a1", 32),
	})
	if err != nil {
		t.Fatalf("NewTarget() error = %v", err)
	}
	result, err := rotator.Run(
		t.Context(),
		plan,
		target,
		func(context.Context, Attempt) AttemptOutcome {
			return AttemptOutcome{
				Kind:        OutcomeSwitchSource,
				FailureKind: downloadFailure.Kind,
				Err:         downloadFailure,
			}
		},
	)
	var rotationFailure *RotationError
	var gotDownloadFailure *DownloadFailure
	var gotTyped *testTypedError
	if !errors.As(err, &rotationFailure) ||
		!errors.As(err, &gotDownloadFailure) ||
		gotDownloadFailure != downloadFailure ||
		!errors.As(err, &gotTyped) ||
		gotTyped != typedCause ||
		!errors.Is(err, errTestSecret) {
		t.Fatalf(
			"Rotator error = %v, want RotationError and original typed downloader chain",
			err,
		)
	}
	publicTexts := []string{err.Error()}
	for _, report := range result.Reports {
		if report.FailureKind != FailureNetwork ||
			report.Error != "attempt failed: network" {
			t.Fatalf("AttemptReport = %#v, want stable network report", report)
		}
		publicTexts = append(publicTexts, report.Error)
	}
	for _, cause := range rotationFailure.Unwrap() {
		publicTexts = append(publicTexts, cause.Error())
	}
	for _, text := range publicTexts {
		for _, secret := range []string{
			"user:password",
			"url-secret",
			"callback-secret",
			"fragment",
			"example.invalid",
			"https://",
		} {
			if strings.Contains(text, secret) {
				t.Fatalf("public Rotator text %q contains %q", text, secret)
			}
		}
	}
}

func TestDownloader_CleanupDeadlinePreservesMainAndDeadline(t *testing.T) {
	mainErr := errors.New("network failure")
	session := successfulRecordingSession()
	session.abort = func(ctx context.Context) (filesystem.AbortResult, error) {
		<-ctx.Done()
		return filesystem.AbortResult{Removed: false}, ctx.Err()
	}
	client := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return nil, mainErr
	}}
	cleanup := func(operationCtx context.Context) (context.Context, context.CancelFunc) {
		return context.WithDeadline(
			context.WithoutCancel(operationCtx),
			time.Unix(1, 0),
		)
	}
	downloader := downloaderForTransactionTest(t, session, client, cleanup)
	_, err := downloader.Download(t.Context(), requestForBytes([]byte("abc")))
	var failure *DownloadFailure
	if !errors.As(err, &failure) || failure.Kind != FailureNetwork {
		t.Fatalf("Download() error = %v, want FailureNetwork", err)
	}
	if !errors.Is(failure, mainErr) ||
		!errors.Is(failure, context.DeadlineExceeded) {
		t.Fatalf("failure = %#v, want main and cleanup deadline chains", failure)
	}
}

type terminalBody struct {
	content []byte
	err     error
	done    bool
}

func (b *terminalBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	return copy(p, b.content), b.err
}

func (b *terminalBody) Close() error {
	return nil
}

type orderedBody struct {
	reader *strings.Reader
	events chan string
}

func (b *orderedBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *orderedBody) Close() error {
	b.events <- "close"
	return nil
}

func responseClient(
	t *testing.T,
	content []byte,
	contentLength int64,
	status int,
) httpClient {
	t.Helper()
	return responseWithBodyClient(
		t,
		io.NopCloser(bytes.NewReader(content)),
		contentLength,
		status,
	)
}

func responseWithBodyClient(
	t *testing.T,
	body io.ReadCloser,
	contentLength int64,
	status int,
) httpClient {
	t.Helper()
	return &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    status,
			ContentLength: contentLength,
			Body:          body,
			Request:       request,
		}, nil
	}}
}

func successfulRecordingSession() *recordingSession {
	return &recordingSession{
		fakeDownloadSession: fakeDownloadSession{
			path:     `C:\app\runtime\cache\downloads\uv.zip`,
			partPath: `C:\app\runtime\cache\downloads\uv.zip.part`,
		},
		publishResult: filesystem.PublishResult{Published: true},
		abortResult:   filesystem.AbortResult{Removed: true},
	}
}

func downloaderForTransactionTest(
	t *testing.T,
	session downloadSession,
	client httpClient,
	cleanup cleanupContextFactory,
) *Downloader {
	t.Helper()
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v, want nil", err)
	}
	dependencies := testDependencies(
		&fakeSessionFactory{session: session},
		client,
	)
	if cleanup != nil {
		dependencies.cleanup = cleanup
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

func requestForBytes(content []byte) DownloadRequest {
	digest := sha256.Sum256(content)
	return DownloadRequest{
		URL:            "https://downloads.example.invalid/uv.zip?signature=request-secret",
		FileName:       "uv.zip",
		ExpectedSize:   int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]),
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestDownloader_ComponentCleanRetryNeverUsesRange(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DownloadFiles handle semantics require Windows")
	}
	content := []byte("clean-retry-content")
	var attempts atomic.Int32
	firstStarted := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Header.Get("Range") != "" ||
			request.Header.Get("If-Range") != "" {
			t.Errorf(
				"resume headers = Range:%q If-Range:%q, want empty",
				request.Header.Get("Range"),
				request.Header.Get("If-Range"),
			)
		}
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-request.Context().Done()
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	layout := newComponentLayout(t)
	if err := os.MkdirAll(layout.DownloadCacheDir(), 0o700); err != nil {
		t.Fatalf("MkdirAll(download cache) error = %v", err)
	}
	stalePart, err := layout.DownloadPartFile("uv.zip")
	if err != nil {
		t.Fatalf("DownloadPartFile(stale) error = %v", err)
	}
	if err := os.WriteFile(stalePart, []byte("stale-part"), 0o600); err != nil {
		t.Fatalf("WriteFile(stale part) error = %v", err)
	}
	downloader := newFilesystemDownloader(
		t,
		layout,
		trustedClientForServers(t, server),
	)
	request := requestForBytes(content)
	request.URL = server.URL + "/artifact?signature=retry-secret"
	ctx, cancel := context.WithCancel(t.Context())
	firstResult := make(chan error, 1)
	go func() {
		_, err := downloader.Download(ctx, request)
		firstResult <- err
	}()
	waitSignal(t, firstStarted)
	cancel()
	firstErr := waitValue(t, firstResult)
	var failure *DownloadFailure
	if !errors.As(firstErr, &failure) || failure.Kind != FailureCancelled {
		t.Fatalf("first Download() error = %v, want FailureCancelled", firstErr)
	}
	partPath, err := layout.DownloadPartFile(request.FileName)
	if err != nil {
		t.Fatalf("DownloadPartFile() error = %v, want nil", err)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled part Stat() error = %v, want not exist", err)
	}

	result, err := downloader.Download(t.Context(), request)
	if err != nil {
		t.Fatalf("retry Download() error = %v, want nil", err)
	}
	got, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("ReadFile(result.Path) error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("final content = %q, want %q", got, content)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempt count = %d, want 2", attempts.Load())
	}
}

func TestDownloader_ComponentFinalRaceIsNoReplaceAndPartIsCleaned(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DownloadFiles handle semantics require Windows")
	}
	content := []byte("verified-content")
	competitor := []byte("competitor-content")
	server := tlsArtifactServer(t, content)
	defer server.Close()
	layout := newComponentLayout(t)
	downloader := newFilesystemDownloader(
		t,
		layout,
		trustedClientForServers(t, server),
	)
	request := requestForBytes(content)
	request.URL = server.URL + "/artifact"
	finalPath, err := layout.DownloadFile(request.FileName)
	if err != nil {
		t.Fatalf("DownloadFile() error = %v", err)
	}
	var once sync.Once
	request.Progress = func(progress DownloadProgress) error {
		if progress.Received == progress.Total {
			once.Do(func() {
				if err := os.WriteFile(finalPath, competitor, 0o600); err != nil {
					t.Fatalf("create final competitor: %v", err)
				}
			})
		}
		return nil
	}
	result, err := downloader.Download(t.Context(), request)
	var failure *DownloadFailure
	if !errors.As(err, &failure) ||
		failure.Kind != FailureDestinationOccupied ||
		failure.Published {
		t.Fatalf("Download() result/error = %#v/%v, want unpublished destination occupied", result, err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile(final competitor) error = %v", err)
	}
	if !bytes.Equal(got, competitor) {
		t.Fatalf("final competitor = %q, want %q", got, competitor)
	}
	partPath, err := layout.DownloadPartFile(request.FileName)
	if err != nil {
		t.Fatalf("DownloadPartFile() error = %v", err)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("part Stat() error = %v, want not exist", err)
	}
}

func TestDownloader_ComponentPartIdentityCannotBeReplaced(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DownloadFiles handle semantics require Windows")
	}
	content := []byte("identity-content")
	server := tlsArtifactServer(t, content)
	defer server.Close()
	layout := newComponentLayout(t)
	downloader := newFilesystemDownloader(
		t,
		layout,
		trustedClientForServers(t, server),
	)
	request := requestForBytes(content)
	request.URL = server.URL + "/artifact"
	partPath, err := layout.DownloadPartFile(request.FileName)
	if err != nil {
		t.Fatalf("DownloadPartFile() error = %v", err)
	}
	replacementPath := partPath + ".replacement"
	replacementAttempt := make(chan error, 1)
	var once sync.Once
	request.Progress = func(progress DownloadProgress) error {
		if progress.Received == progress.Total {
			once.Do(func() {
				replacementAttempt <- os.Rename(partPath, replacementPath)
			})
		}
		return nil
	}
	result, err := downloader.Download(t.Context(), request)
	if err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	if replaceErr := waitValue(t, replacementAttempt); replaceErr == nil {
		t.Fatal("os.Rename(part, replacement) error = nil, want sharing rejection")
	}
	got, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("ReadFile(result.Path) error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("published content = %q, want %q", got, content)
	}
	if _, err := os.Stat(replacementPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement Stat() error = %v, want not exist", err)
	}
}

func TestDownloader_ComponentJunctionAtEveryAncestorFailsBeforeNetwork(
	t *testing.T,
) {
	if runtime.GOOS != "windows" {
		t.Fatal("ordinary Junction acceptance must run on Windows")
	}
	cases := []struct {
		name string
		path func(*config.Layout) string
	}{
		{name: "app-root", path: func(layout *config.Layout) string {
			return layout.AppRoot()
		}},
		{name: "runtime", path: func(layout *config.Layout) string {
			return layout.RuntimeDir()
		}},
		{name: "cache", path: func(layout *config.Layout) string {
			return layout.RuntimeCacheDir()
		}},
		{name: "downloads", path: func(layout *config.Layout) string {
			return layout.DownloadCacheDir()
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			base := t.TempDir()
			appRoot := filepath.Join(base, "app")
			layout, err := config.NewLayout(appRoot, base)
			if err != nil {
				t.Fatalf("config.NewLayout() error = %v", err)
			}
			external := filepath.Join(base, "external")
			if err := os.MkdirAll(external, 0o700); err != nil {
				t.Fatalf("MkdirAll(external) error = %v", err)
			}
			markerPath := filepath.Join(external, "marker.txt")
			if err := os.WriteFile(
				markerPath,
				[]byte("outside"),
				0o600,
			); err != nil {
				t.Fatalf("WriteFile(marker) error = %v", err)
			}
			junctionPath := testCase.path(layout)
			if err := os.MkdirAll(
				filepath.Dir(junctionPath),
				0o700,
			); err != nil {
				t.Fatalf("MkdirAll(junction parent) error = %v", err)
			}
			createOrdinaryJunction(t, junctionPath, external)

			files, downloadErr := filesystem.NewDownloadFiles(layout)
			client := &fakeHTTPClient{}
			if downloadErr == nil {
				options, err := resolveDownloaderOptions(nil)
				if err != nil {
					t.Fatalf("resolveDownloaderOptions() error = %v", err)
				}
				dependencies := testDependencies(
					filesystemSessions{files: files},
					client,
				)
				downloader, err := newDownloaderWithDependencies(
					layout,
					options,
					dependencies,
				)
				if err != nil {
					t.Fatalf(
						"newDownloaderWithDependencies() error = %v",
						err,
					)
				}
				_, downloadErr = downloader.Download(
					t.Context(),
					requestForBytes([]byte("abc")),
				)
			}
			if downloadErr == nil {
				t.Fatal("Junction ancestor error = nil, want failure")
			}
			if client.Calls() != 0 {
				t.Fatalf("network calls = %d, want 0", client.Calls())
			}
			got, err := os.ReadFile(markerPath)
			if err != nil || string(got) != "outside" {
				t.Fatalf(
					"external marker = %q, error = %v, want outside",
					got,
					err,
				)
			}
		})
	}
}

func createOrdinaryJunction(t *testing.T, link, target string) {
	t.Helper()
	const script = `
$ErrorActionPreference = "Stop"
$link = $env:AUTO_MAS_TEST_JUNCTION_LINK
$target = $env:AUTO_MAS_TEST_JUNCTION_TARGET
New-Item -ItemType Junction -Path $link -Target $target | Out-Null
`
	command := exec.CommandContext(
		t.Context(),
		"pwsh",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_TEST_JUNCTION_LINK="+link,
		"AUTO_MAS_TEST_JUNCTION_TARGET="+target,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf(
			"create ordinary Junction %q -> %q: %v\n%s",
			link,
			target,
			err,
			output,
		)
	}
}

func TestDownloader_ComponentRejects206AndCleansPart(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DownloadFiles handle semantics require Windows")
	}
	content := []byte("partial")
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content)
	}))
	defer server.Close()
	layout := newComponentLayout(t)
	downloader := newFilesystemDownloader(
		t,
		layout,
		trustedClientForServers(t, server),
	)
	request := requestForBytes(content)
	request.URL = server.URL + "/artifact"
	_, err := downloader.Download(t.Context(), request)
	var failure *DownloadFailure
	if !errors.As(err, &failure) ||
		failure.Kind != FailureHTTPStatus ||
		failure.StatusCode != http.StatusPartialContent {
		t.Fatalf("Download() error = %v, want HTTP 206 failure", err)
	}
	partPath, err := layout.DownloadPartFile(request.FileName)
	if err != nil {
		t.Fatalf("DownloadPartFile() error = %v", err)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("part Stat() error = %v, want not exist", err)
	}
}

func TestDownloader_ComponentTLSChecksumMismatchCleansRealPart(
	t *testing.T,
) {
	if runtime.GOOS != "windows" {
		t.Fatal("real DownloadFiles acceptance must run on Windows")
	}
	content := []byte("checksum-mismatch-content")
	server := tlsArtifactServer(t, content)
	defer server.Close()
	layout := newComponentLayout(t)
	downloader := newFilesystemDownloader(
		t,
		layout,
		trustedClientForServers(t, server),
	)
	request := requestForBytes(content)
	request.URL = server.URL + "/artifact?signature=checksum-secret"
	request.ExpectedSHA256 = strings.Repeat("0", 64)

	result, err := downloader.Download(t.Context(), request)
	var failure *DownloadFailure
	if result != (DownloadResult{}) ||
		!errors.As(err, &failure) ||
		failure.Kind != FailureChecksumMismatch ||
		failure.Published {
		t.Fatalf(
			"Download() result/error = %#v/%v, want unpublished checksum mismatch",
			result,
			err,
		)
	}
	finalPath, finalErr := layout.DownloadFile(request.FileName)
	if finalErr != nil {
		t.Fatalf("DownloadFile() error = %v", finalErr)
	}
	partPath, partErr := layout.DownloadPartFile(request.FileName)
	if partErr != nil {
		t.Fatalf("DownloadPartFile() error = %v", partErr)
	}
	if _, statErr := os.Stat(finalPath); !errors.Is(
		statErr,
		os.ErrNotExist,
	) {
		t.Fatalf("final Stat() error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(partPath); !errors.Is(
		statErr,
		os.ErrNotExist,
	) {
		t.Fatalf("part Stat() error = %v, want not exist", statErr)
	}
	if strings.Contains(
		failure.Error()+" "+errorText(failure.Err),
		"checksum-secret",
	) {
		t.Fatalf("public failure leaked signed URL: %v", failure)
	}
}

func newComponentLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func newFilesystemDownloader(
	t *testing.T,
	layout *config.Layout,
	client httpClient,
) *Downloader {
	t.Helper()
	files, err := filesystem.NewDownloadFiles(layout)
	if err != nil {
		t.Fatalf("filesystem.NewDownloadFiles() error = %v", err)
	}
	options, err := resolveDownloaderOptions(nil)
	if err != nil {
		t.Fatalf("resolveDownloaderOptions() error = %v", err)
	}
	downloader, err := newDownloaderWithDependencies(
		layout,
		options,
		downloaderDependencies{
			sessions: filesystemSessions{files: files},
			client:   client,
			clock:    time.Now,
			timers: func(delay time.Duration) timer {
				return &runtimeTimer{timer: time.NewTimer(delay)}
			},
			cleanup: func(operationCtx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(
					context.WithoutCancel(operationCtx),
					cleanupTimeout,
				)
			},
		},
	)
	if err != nil {
		t.Fatalf("newDownloaderWithDependencies() error = %v", err)
	}
	return downloader
}

func tlsArtifactServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Header.Get("Range") != "" {
			t.Errorf("Range = %q, want empty", request.Header.Get("Range"))
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = writer.Write(content)
	}))
}
