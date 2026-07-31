package mirror

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
