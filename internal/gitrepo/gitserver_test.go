package gitrepo

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

const gitFixtureTimeout = 10 * time.Second

type gitFixtureCommit struct {
	label   string
	version string
}

type gitFixtureRepository struct {
	repository *git.Repository
	commits    map[string]plumbing.Hash
}

func newGitFixtureRepository(
	t *testing.T,
	commits ...gitFixtureCommit,
) *gitFixtureRepository {
	t.Helper()
	if len(commits) == 0 {
		t.Fatal("git fixture commits are empty")
	}
	seedPath := filepath.Join(t.TempDir(), "seed")
	seed, err := git.PlainInit(seedPath, false)
	if err != nil {
		t.Fatalf("PlainInit(seed) error = %v", err)
	}
	worktree, err := seed.Worktree()
	if err != nil {
		t.Fatalf("Worktree(seed) error = %v", err)
	}
	hashes := make(map[string]plumbing.Hash, len(commits))
	for index, commit := range commits {
		if commit.label == "" || commit.version == "" {
			t.Fatalf("commit[%d] = %#v, want label and version", index, commit)
		}
		versionPath := filepath.Join(seedPath, "res", "version.json")
		if err := os.MkdirAll(filepath.Dir(versionPath), 0o700); err != nil {
			t.Fatalf("MkdirAll(res) error = %v", err)
		}
		if err := os.WriteFile(
			versionPath,
			[]byte(`{"version":"`+commit.version+`"}`),
			0o600,
		); err != nil {
			t.Fatalf("WriteFile(version) error = %v", err)
		}
		if err := os.WriteFile(
			filepath.Join(seedPath, "fixture-marker.txt"),
			[]byte(commit.label),
			0o600,
		); err != nil {
			t.Fatalf("WriteFile(marker) error = %v", err)
		}
		for _, path := range []string{"res/version.json", "fixture-marker.txt"} {
			if _, err := worktree.Add(path); err != nil {
				t.Fatalf("Add(%q) error = %v", path, err)
			}
		}
		hash, err := worktree.Commit("fixture "+commit.label, &git.CommitOptions{
			Author: &object.Signature{
				Name:  "AUTO-MAS component test",
				Email: "component@example.invalid",
				When:  time.Date(2026, 8, 6, 0, index, 0, 0, time.UTC),
			},
		})
		if err != nil {
			t.Fatalf("Commit(%q) error = %v", commit.label, err)
		}
		if _, exists := hashes[commit.label]; exists {
			t.Fatalf("duplicate fixture commit label %q", commit.label)
		}
		hashes[commit.label] = hash
	}

	barePath := filepath.Join(t.TempDir(), "fixture.git")
	bare, err := git.PlainClone(barePath, true, &git.CloneOptions{
		URL:  seedPath,
		Tags: git.AllTags,
	})
	if err != nil {
		t.Fatalf("PlainClone(bare) error = %v", err)
	}
	if _, err := bare.Worktree(); !errors.Is(err, git.ErrIsBareRepository) {
		t.Fatalf("Worktree(bare) error = %v, want ErrIsBareRepository", err)
	}
	removeAllFixtureReferences(t, bare)
	return &gitFixtureRepository{repository: bare, commits: hashes}
}

func removeAllFixtureReferences(t *testing.T, repository *git.Repository) {
	t.Helper()
	iterator, err := repository.References()
	if err != nil {
		t.Fatalf("References() error = %v", err)
	}
	var names []plumbing.ReferenceName
	for {
		reference, nextErr := iterator.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			iterator.Close()
			t.Fatalf("References().Next() error = %v", nextErr)
		}
		names = append(names, reference.Name())
	}
	iterator.Close()
	for _, name := range names {
		if err := repository.Storer.RemoveReference(name); err != nil {
			t.Fatalf("RemoveReference(%q) error = %v", name, err)
		}
	}
}

func (r *gitFixtureRepository) hash(t *testing.T, label string) plumbing.Hash {
	t.Helper()
	hash, ok := r.commits[label]
	if !ok {
		t.Fatalf("fixture commit %q is missing", label)
	}
	return hash
}

func (r *gitFixtureRepository) setBranch(
	t *testing.T,
	version string,
	label string,
) {
	t.Helper()
	branch := plumbing.NewBranchReferenceName("release/" + version)
	if err := r.repository.Storer.SetReference(
		plumbing.NewHashReference(branch, r.hash(t, label)),
	); err != nil {
		t.Fatalf("SetReference(%q) error = %v", branch, err)
	}
	if err := r.repository.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, branch),
	); err != nil {
		t.Fatalf("SetReference(HEAD) error = %v", err)
	}
}

func (r *gitFixtureRepository) addTag(
	t *testing.T,
	name string,
	label string,
) {
	t.Helper()
	tag := plumbing.NewTagReferenceName(name)
	if err := r.repository.Storer.SetReference(
		plumbing.NewHashReference(tag, r.hash(t, label)),
	); err != nil {
		t.Fatalf("SetReference(%q) error = %v", tag, err)
	}
}

type gitFixtureBarrier struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newGitFixtureBarrier() *gitFixtureBarrier {
	return &gitFixtureBarrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *gitFixtureBarrier) wait(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *gitFixtureBarrier) releaseRequest() {
	if b == nil {
		return
	}
	b.releaseOnce.Do(func() { close(b.release) })
}

type gitFixtureFault struct {
	discovery         *gitFixtureBarrier
	pack              *gitFixtureBarrier
	response          *gitFixtureBarrier
	responseRead      *gitFixtureBarrier
	interruptResponse bool
}

type gitFixtureStats struct {
	discoveries int
	packs       int
	depths      []int
}

type gitHTTPSFixture struct {
	mu sync.Mutex

	server     *httptest.Server
	loader     gitserver.MapLoader
	transport  transport.Transport
	repository map[string]*gitFixtureRepository
	faults     map[string]gitFixtureFault
	stats      map[string]gitFixtureStats
	errors     []error
	caBundle   []byte
}

func newGitHTTPSFixture(
	t *testing.T,
	repositories map[string]*gitFixtureRepository,
) *gitHTTPSFixture {
	t.Helper()
	if len(repositories) == 0 {
		t.Fatal("git HTTPS fixture repositories are empty")
	}
	fixture := &gitHTTPSFixture{
		loader:     make(gitserver.MapLoader, len(repositories)),
		repository: repositories,
		faults:     make(map[string]gitFixtureFault, len(repositories)),
		stats:      make(map[string]gitFixtureStats, len(repositories)),
	}
	fixture.transport = gitserver.NewServer(fixture.loader)
	server := httptest.NewUnstartedServer(http.HandlerFunc(fixture.serveHTTP))
	server.EnableHTTP2 = false
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	fixture.server = server
	certificate := server.Certificate()
	if certificate == nil {
		server.Close()
		t.Fatal("TLS server certificate is nil")
	}
	fixture.caBundle = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate.Raw,
	})
	for name, repository := range repositories {
		endpoint, err := transport.NewEndpoint(fixture.repositoryURL(name))
		if err != nil {
			server.Close()
			t.Fatalf("NewEndpoint(%q) error = %v", name, err)
		}
		fixture.loader[endpoint.String()] = repository.repository.Storer
	}
	t.Cleanup(func() {
		for _, fault := range fixture.faults {
			fault.discovery.releaseRequest()
			fault.pack.releaseRequest()
			fault.response.releaseRequest()
			fault.responseRead.releaseRequest()
		}
		server.Close()
	})
	return fixture
}

func (f *gitHTTPSFixture) repositoryURL(name string) string {
	return f.server.URL + "/" + name + ".git"
}

func (f *gitHTTPSFixture) source(
	t *testing.T,
	name string,
	key string,
	official bool,
) mirror.Source {
	t.Helper()
	if _, ok := f.repository[name]; !ok {
		t.Fatalf("fixture repository %q is missing", name)
	}
	source, err := mirror.NewSource(mirror.KindGit, key, f.repositoryURL(name), official)
	if err != nil {
		t.Fatalf("NewSource(%q) error = %v", name, err)
	}
	return source
}

func (f *gitHTTPSFixture) setFault(name string, fault gitFixtureFault) {
	f.mu.Lock()
	f.faults[name] = fault
	f.mu.Unlock()
}

func (f *gitHTTPSFixture) snapshotStats(name string) gitFixtureStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := f.stats[name]
	stats.depths = append([]int(nil), stats.depths...)
	return stats
}

func (f *gitHTTPSFixture) assertNoServerErrors(t *testing.T) {
	t.Helper()
	if serverErrors := f.snapshotErrors(); len(serverErrors) != 0 {
		t.Fatalf("git HTTPS server errors = %v", errors.Join(serverErrors...))
	}
}

func (f *gitHTTPSFixture) snapshotErrors() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.errors...)
}

func (f *gitHTTPSFixture) recordError(err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	f.errors = append(f.errors, err)
	f.mu.Unlock()
}

func (f *gitHTTPSFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	name, action, ok := parseGitFixturePath(request.URL.Path)
	if !ok {
		http.NotFound(writer, request)
		return
	}
	if _, ok := f.repository[name]; !ok {
		http.NotFound(writer, request)
		return
	}
	f.mu.Lock()
	fault := f.faults[name]
	stats := f.stats[name]
	switch action {
	case "discovery":
		stats.discoveries++
	case "pack":
		stats.packs++
	}
	f.stats[name] = stats
	f.mu.Unlock()

	endpoint, err := transport.NewEndpoint("https://" + request.Host + "/" + name + ".git")
	if err != nil {
		f.failRequest(writer, http.StatusInternalServerError, fmt.Errorf("parse endpoint: %w", err))
		return
	}
	session, err := f.transport.NewUploadPackSession(endpoint, nil)
	if err != nil {
		f.failRequest(writer, http.StatusNotFound, fmt.Errorf("open upload-pack session: %w", err))
		return
	}
	defer func() {
		f.recordError(session.Close())
	}()

	switch action {
	case "discovery":
		f.serveDiscovery(writer, request, session, fault)
	case "pack":
		f.servePack(writer, request, session, name, fault)
	default:
		http.NotFound(writer, request)
	}
}

func (f *gitHTTPSFixture) serveDiscovery(
	writer http.ResponseWriter,
	request *http.Request,
	session transport.UploadPackSession,
	fault gitFixtureFault,
) {
	if request.Method != http.MethodGet ||
		request.URL.Query().Get("service") != transport.UploadPackServiceName {
		f.failRequest(writer, http.StatusBadRequest, errors.New("invalid discovery request"))
		return
	}
	if err := fault.discovery.wait(request.Context()); err != nil {
		return
	}
	advertisement, err := session.AdvertisedReferencesContext(request.Context())
	if err != nil {
		f.failRequest(writer, http.StatusInternalServerError, fmt.Errorf("advertise references: %w", err))
		return
	}
	advertisement.Prefix = [][]byte{
		[]byte("# service=" + transport.UploadPackServiceName),
		pktline.Flush,
	}
	if err := advertisement.Capabilities.Set(capability.Shallow); err != nil {
		f.failRequest(writer, http.StatusInternalServerError, fmt.Errorf("advertise shallow capability: %w", err))
		return
	}
	writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	writer.Header().Set("Cache-Control", "no-cache")
	if err := advertisement.Encode(writer); err != nil {
		f.recordError(fmt.Errorf("encode advertisement: %w", err))
	}
}

func (f *gitHTTPSFixture) servePack(
	writer http.ResponseWriter,
	request *http.Request,
	session transport.UploadPackSession,
	name string,
	fault gitFixtureFault,
) {
	if request.Method != http.MethodPost {
		f.failRequest(writer, http.StatusMethodNotAllowed, errors.New("invalid upload-pack method"))
		return
	}
	uploadRequest := packp.NewUploadPackRequest()
	if err := uploadRequest.UploadRequest.Decode(request.Body); err != nil {
		f.failRequest(writer, http.StatusBadRequest, fmt.Errorf("decode upload-pack request: %w", err))
		return
	}
	depth := -1
	if commits, ok := uploadRequest.Depth.(packp.DepthCommits); ok {
		depth = int(commits)
	}
	f.mu.Lock()
	stats := f.stats[name]
	stats.depths = append(stats.depths, depth)
	f.stats[name] = stats
	f.mu.Unlock()
	response, err := f.uploadPackResponse(request.Context(), session, name, fault, uploadRequest, depth)
	if err != nil {
		f.failRequest(writer, http.StatusInternalServerError, fmt.Errorf("upload pack: %w", err))
		return
	}
	var encoded bytes.Buffer
	if err := response.Encode(&encoded); err != nil {
		f.failRequest(writer, http.StatusInternalServerError, fmt.Errorf("encode upload-pack response: %w", err))
		return
	}
	if err := fault.response.wait(request.Context()); err != nil {
		return
	}
	payload := encoded.Bytes()
	writer.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	writer.Header().Set("Cache-Control", "no-cache")
	if fault.interruptResponse {
		writer.Header().Set("Content-Length", strconv.Itoa(len(payload)+1))
		writer.Header().Set("Connection", "close")
		writer.WriteHeader(http.StatusOK)
		limit := len(payload) / 2
		if limit == 0 && len(payload) > 0 {
			limit = 1
		}
		_, _ = writer.Write(payload[:limit])
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	if fault.responseRead != nil {
		writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		writer.WriteHeader(http.StatusOK)
		limit := len(payload) / 2
		if limit == 0 && len(payload) > 0 {
			limit = 1
		}
		if _, err := writer.Write(payload[:limit]); err != nil {
			f.recordError(fmt.Errorf("write upload-pack response prefix: %w", err))
			return
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		if err := fault.responseRead.wait(request.Context()); err != nil {
			return
		}
		if _, err := writer.Write(payload[limit:]); err != nil {
			f.recordError(fmt.Errorf("write upload-pack response suffix: %w", err))
		}
		return
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(payload); err != nil {
		f.recordError(fmt.Errorf("write upload-pack response: %w", err))
	}
}

func (f *gitHTTPSFixture) uploadPackResponse(
	ctx context.Context,
	session transport.UploadPackSession,
	name string,
	fault gitFixtureFault,
	request *packp.UploadPackRequest,
	depth int,
) (*packp.UploadPackResponse, error) {
	if depth <= 0 {
		return session.UploadPack(ctx, request)
	}
	if depth != 1 {
		return nil, fmt.Errorf("unsupported fixture depth %d", depth)
	}
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate depth-one request: %w", err)
	}
	repository := f.repository[name].repository
	objects, err := gitFixtureDepthOneObjects(repository, request.Wants)
	if err != nil {
		return nil, err
	}
	if err := fault.pack.wait(ctx); err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	if _, err := packfile.NewEncoder(&payload, repository.Storer, false).Encode(objects, 0); err != nil {
		return nil, fmt.Errorf("encode depth-one pack: %w", err)
	}
	response := packp.NewUploadPackResponseWithPackfile(
		request,
		io.NopCloser(bytes.NewReader(payload.Bytes())),
	)
	response.Shallows = append([]plumbing.Hash(nil), request.Wants...)
	return response, nil
}

func gitFixtureDepthOneObjects(
	repository *git.Repository,
	wants []plumbing.Hash,
) ([]plumbing.Hash, error) {
	seen := make(map[plumbing.Hash]struct{})
	objects := make([]plumbing.Hash, 0, len(wants)*4)
	add := func(hash plumbing.Hash) {
		if _, exists := seen[hash]; exists {
			return
		}
		seen[hash] = struct{}{}
		objects = append(objects, hash)
	}
	for _, want := range wants {
		commit, err := repository.CommitObject(want)
		if err != nil {
			return nil, fmt.Errorf("open wanted commit %s: %w", want, err)
		}
		add(commit.Hash)
		tree, err := commit.Tree()
		if err != nil {
			return nil, fmt.Errorf("open wanted tree %s: %w", commit.TreeHash, err)
		}
		add(tree.Hash)
		walker := object.NewTreeWalker(tree, true, nil)
		for {
			_, entry, nextErr := walker.Next()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				walker.Close()
				return nil, fmt.Errorf("walk wanted tree %s: %w", tree.Hash, nextErr)
			}
			if entry.Mode != filemode.Submodule {
				add(entry.Hash)
			}
		}
		walker.Close()
	}
	return objects, nil
}

func (f *gitHTTPSFixture) failRequest(
	writer http.ResponseWriter,
	status int,
	err error,
) {
	f.recordError(err)
	http.Error(writer, http.StatusText(status), status)
}

func parseGitFixturePath(path string) (name string, action string, ok bool) {
	for suffix, candidateAction := range map[string]string{
		"/info/refs":       "discovery",
		"/git-upload-pack": "pack",
	} {
		if !strings.HasSuffix(path, suffix) {
			continue
		}
		base := strings.TrimSuffix(path, suffix)
		if !strings.HasPrefix(base, "/") || !strings.HasSuffix(base, ".git") {
			return "", "", false
		}
		name = strings.TrimSuffix(strings.TrimPrefix(base, "/"), ".git")
		if name == "" || strings.Contains(name, "/") {
			return "", "", false
		}
		return name, candidateAction, true
	}
	return "", "", false
}

func gitFixturePlan(
	t *testing.T,
	sources []mirror.Source,
	preferred string,
) mirror.Plan {
	t.Helper()
	defaults, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	allSources := append([]mirror.Source(nil), sources...)
	for _, kind := range mirror.AllKinds() {
		if kind == mirror.KindGit {
			continue
		}
		allSources = append(allSources, defaults.Sources(kind)...)
	}
	catalog, err := mirror.NewCatalog(allSources)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	preferredSources := make(map[mirror.Kind]string)
	if preferred != "" {
		preferredSources[mirror.KindGit] = preferred
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: preferredSources})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := mirror.BuildPlan(catalog, policy, mirror.KindGit)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	return plan
}

func waitForGitFixtureBarrier(t *testing.T, barrier *gitFixtureBarrier) {
	t.Helper()
	select {
	case <-barrier.entered:
	case <-time.After(gitFixtureTimeout):
		t.Fatal("timed out waiting for Git fixture barrier")
	}
}

func TestGitHTTPSServer_RequiresInjectedCA(t *testing.T) {
	repository := newGitFixtureRepository(t, gitFixtureCommit{label: "release", version: "v1.0.0"})
	repository.setBranch(t, "v1.0.0", "release")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)

	if _, err := (goGitClient{}).ListReferences(t.Context(), source.BaseURL(), nil); err == nil {
		t.Fatal("ListReferences() without test CA error = nil, want TLS validation failure")
	}
	references, err := (goGitClient{}).ListReferences(
		t.Context(),
		source.BaseURL(),
		server.caBundle,
	)
	if err != nil {
		t.Fatalf("ListReferences() with test CA error = %v", err)
	}
	if !containsBranch(references, "release/v1.0.0") {
		t.Fatalf("references = %#v, want release/v1.0.0", references)
	}
	server.assertNoServerErrors(t)
}
