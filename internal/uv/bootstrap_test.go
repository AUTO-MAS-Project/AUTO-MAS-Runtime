package uv

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestBootstrap_CacheHitAndVersionVerification(t *testing.T) {
	layout := newUVTestLayout(t)
	artifact := testArtifact("cached archive")
	executable, err := layout.UVExecutable(artifact.Version)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("fake uv"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	checker := &fakeVersionChecker{}
	downloader := &fakeDownloader{err: errors.New("cache hit must not download")}
	bootstrapper := newTestBootstrapper(t, layout, artifact, downloader, checker, nil, nil)
	policy := testMirrorPolicy(t)

	got, err := bootstrapper.Ensure(t.Context(), testOperationID, policy)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if got != executable {
		t.Fatalf("Ensure() path = %q, want %q", got, executable)
	}
	if checker.calls != 1 {
		t.Fatalf("version checks = %d, want 1", checker.calls)
	}
	if downloader.calls != 0 {
		t.Fatalf("downloads = %d, want 0", downloader.calls)
	}
}

func TestBootstrap_ChecksumMismatch(t *testing.T) {
	layout := newUVTestLayout(t)
	artifact := testArtifact("expected archive")
	downloader := &fakeDownloader{payload: []byte("tampered archive")}
	bootstrapper := newTestBootstrapper(t, layout, artifact, downloader, &fakeVersionChecker{}, nil, nil)

	_, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Ensure() error = %T %v, want uv Error", err, err)
	}
	if operationErr.Code() != protocol.CodeUVChecksumMismatch {
		t.Fatalf("Ensure() code = %q, want %q", operationErr.Code(), protocol.CodeUVChecksumMismatch)
	}
	if downloader.request.ExpectedSize != 0 || !downloader.request.AllowUnknownSize {
		t.Fatalf("download size request = %#v, want response-sized request", downloader.request)
	}
}

func TestBootstrap_AtomicPublish(t *testing.T) {
	layout := newUVTestLayout(t)
	archiveBytes := makeUVArchive(t)
	artifact := testArtifact(string(archiveBytes))
	downloader := &fakeDownloader{payload: archiveBytes}
	checker := &fakeVersionChecker{}
	extractor := &fakeExtractor{}
	publisher := &fakePublisher{}
	bootstrapper := newTestBootstrapper(t, layout, artifact, downloader, checker, extractor, publisher)

	got, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	want, err := layout.UVExecutable(artifact.Version)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if got != want {
		t.Fatalf("Ensure() path = %q, want %q", got, want)
	}
	if !publisher.result.MutationApplied {
		t.Fatal("publisher did not report the atomic mutation")
	}
	if publisher.request.Version != artifact.Version ||
		publisher.request.OperationID != testOperationID ||
		publisher.request.Destination != filepath.Dir(want) {
		t.Fatalf("publish request = %#v, want versioned destination", publisher.request)
	}
	if extractor.calls != 1 || checker.calls != 1 {
		t.Fatalf("extractor/checker calls = %d/%d, want 1/1", extractor.calls, checker.calls)
	}
}

func TestBootstrap_PostPublishVersionFailureIsCommitted(t *testing.T) {
	layout := newUVTestLayout(t)
	archiveBytes := makeUVArchive(t)
	artifact := testArtifact(string(archiveBytes))
	checker := &fakeVersionChecker{err: newError(
		protocol.CodeUVVersionMismatch,
		protocol.StageUVVerify,
		"uv 版本校验失败",
		map[string]any{"expectedVersion": artifact.Version},
		errors.New("fake post-publish mismatch"),
	)}
	bootstrapper := newTestBootstrapper(
		t,
		layout,
		artifact,
		&fakeDownloader{payload: archiveBytes},
		checker,
		&fakeExtractor{},
		&fakePublisher{},
	)

	_, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Ensure() error = %T %v, want uv Error", err, err)
	}
	if !operationErr.Committed() {
		t.Fatalf("Ensure() committed = false, want true")
	}
	if operationErr.Details()["committed"] != true {
		t.Fatalf("Ensure() details = %#v, want committed=true", operationErr.Details())
	}
}

func TestBootstrap_VersionSpoofIsRejected(t *testing.T) {
	layout := newUVTestLayout(t)
	artifact := testArtifact("archive")
	executable, err := layout.UVExecutable(artifact.Version)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("spoof"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	checker := &fakeVersionChecker{err: newError(
		protocol.CodeUVVersionMismatch,
		protocol.StageUVVerify,
		"uv 版本校验失败",
		map[string]any{},
		errors.New("spoofed version"),
	)}
	bootstrapper := newTestBootstrapper(t, layout, artifact, &fakeDownloader{err: errors.New("stop")}, checker, nil, nil)

	_, err = bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("Ensure() error = %T %v, want structured error", err, err)
	}
	if checker.calls != 1 {
		t.Fatalf("version checks = %d, want 1", checker.calls)
	}
}

func TestBootstrap_RealZipExtractorPublishesOnlyUV(t *testing.T) {
	layout := newUVTestLayout(t)
	archiveBytes := makeUVArchive(t)
	artifact := testArtifact(string(archiveBytes))
	downloader := &fakeDownloader{payload: archiveBytes}
	publisher := &fakePublisher{}
	bootstrapper := newTestBootstrapper(t, layout, artifact, downloader, &fakeVersionChecker{}, zipExtractor{}, publisher)

	if _, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t)); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	uvPath := filepath.Join(publisher.request.Source, "uv.exe")
	contents, err := os.ReadFile(uvPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", uvPath, err)
	}
	if string(contents) != "fake uv" {
		t.Fatalf("extracted uv.exe = %q, want fixture content", contents)
	}
}

func TestZipExtractor_RejectsUnsafeEntries(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "uv.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("../uv.exe")
	if err != nil {
		t.Fatalf("Create zip entry: %v", err)
	}
	if _, err := entry.Write([]byte("unsafe")); err != nil {
		t.Fatalf("Write zip entry: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	err = (zipExtractor{}).Extract(t.Context(), archivePath, t.TempDir())
	if err == nil {
		t.Fatal("Extract() error = nil, want unsafe path rejection")
	}
}

const testOperationID = "01J00000000000000000000000"

func newUVTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return layout
}

func testArtifact(payload string) Artifact {
	digest := sha256.Sum256([]byte(payload))
	return Artifact{
		Version: "0.0.1",
		Kind:    mirror.KindUV,
		Name:    "uv-test.zip",
		SHA256:  hex.EncodeToString(digest[:]),
	}
}

func testMirrorPolicy(t *testing.T) mirror.Policy {
	t.Helper()
	policy, err := mirror.NewPolicy(mirror.PolicySpec{
		Preferred: map[mirror.Kind]string{},
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	return policy
}

func newTestBootstrapper(
	t *testing.T,
	layout *config.Layout,
	artifact Artifact,
	downloader *fakeDownloader,
	checker *fakeVersionChecker,
	extractor ArchiveExtractor,
	publisher Publisher,
) *Bootstrapper {
	t.Helper()
	options := []BootstrapOption{
		WithArtifact(artifact),
		WithDownloader(func() *fakeDownloader {
			downloader.path = mustDownloadPath(t, layout, artifact.Name)
			return downloader
		}()),
		WithVersionChecker(checker),
		WithBootstrapRotator(fakeRotationRunner{}),
	}
	if extractor != nil {
		options = append(options, WithArchiveExtractor(extractor))
	}
	if publisher != nil {
		options = append(options, WithPublisher(publisher))
	}
	bootstrapper, err := NewBootstrapper(layout, options...)
	if err != nil {
		t.Fatalf("NewBootstrapper() error = %v", err)
	}
	return bootstrapper
}

type fakeDownloader struct {
	payload []byte
	err     error
	calls   int
	request mirror.DownloadRequest
	path    string
}

func (f *fakeDownloader) Download(
	ctx context.Context,
	request mirror.DownloadRequest,
) (mirror.DownloadResult, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return mirror.DownloadResult{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return mirror.DownloadResult{}, err
	}
	if f.path == "" {
		return mirror.DownloadResult{}, errors.New("test downloader path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return mirror.DownloadResult{}, err
	}
	if err := os.WriteFile(f.path, f.payload, 0o600); err != nil {
		return mirror.DownloadResult{}, err
	}
	return mirror.DownloadResult{Path: f.path}, nil
}

func mustDownloadPath(t *testing.T, layout *config.Layout, name string) string {
	t.Helper()
	path, err := layout.DownloadFile(name)
	if err != nil {
		t.Fatalf("DownloadFile() error = %v", err)
	}
	return path
}

type fakeVersionChecker struct {
	calls int
	err   error
}

func (f *fakeVersionChecker) Check(context.Context, string, string, LineFunc) error {
	f.calls++
	return f.err
}

type fakeExtractor struct{ calls int }

func (f *fakeExtractor) Extract(
	ctx context.Context,
	_, stagingDir string,
) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stagingDir, "uv.exe"), []byte("uv"), 0o700)
}

type fakePublisher struct {
	request PublishRequest
	result  PublishResult
}

func (f *fakePublisher) Publish(_ context.Context, request PublishRequest) (PublishResult, error) {
	f.request = request
	if err := os.MkdirAll(request.Destination, 0o700); err != nil {
		return PublishResult{}, err
	}
	if err := os.WriteFile(filepath.Join(request.Destination, "uv.exe"), []byte("fake uv"), 0o700); err != nil {
		return PublishResult{}, err
	}
	f.result = PublishResult{MutationApplied: true}
	return f.result, nil
}

type fakeRotationRunner struct{}

func (fakeRotationRunner) Run(
	ctx context.Context,
	plan mirror.Plan,
	target mirror.Target,
	attempt mirror.AttemptFunc,
) (mirror.RotationResult, error) {
	sources := plan.Sources()
	if len(sources) == 0 {
		return mirror.RotationResult{}, errors.New("test rotation plan has no source")
	}
	value := attempt(ctx, mirror.Attempt{
		Source:    sources[0],
		Target:    target,
		SourceTry: 1,
		GlobalTry: 1,
	})
	if value.Kind != mirror.OutcomeSucceeded {
		return mirror.RotationResult{}, value.Err
	}
	return mirror.RotationResult{
		Source: sources[0],
		Reports: []mirror.AttemptReport{{
			Kind:       mirror.KindUV,
			SourceKey:  sources[0].Key(),
			SourceTry:  1,
			GlobalTry:  1,
			Target:     target,
			TargetHash: target.Fingerprint(),
			Outcome:    mirror.OutcomeSucceeded,
		}},
	}, nil
}

func makeUVArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("uv.exe")
	if err != nil {
		t.Fatalf("zip Create() error = %v", err)
	}
	if _, err := io.WriteString(entry, "fake uv"); err != nil {
		t.Fatalf("zip Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	return buffer.Bytes()
}
