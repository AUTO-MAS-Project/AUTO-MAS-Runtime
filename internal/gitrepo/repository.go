package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

const (
	repositoryVersionTreePath = "res/version.json"
	maxRepositoryVersionBytes = 1 << 20
)

var (
	errInvalidRevision       = errors.New("repository revision is invalid")
	errInvalidRepositoryRead = errors.New("repository read request is invalid")
	errInvalidRepositoryID   = errors.New("repository identity is invalid")
	errVersionFileTooLarge   = errors.New("repository version file is too large")
)

// Revision 保存经静态校验的仓库版本、分支、Commit 和来源。
type Revision struct {
	version   string
	branch    string
	commit    string
	sourceKey string
}

func newRevision(target Target, commit string, source mirror.Source) (Revision, error) {
	if err := target.validate(); err != nil ||
		!validCommit(commit) ||
		!validGitSource(source) {
		return Revision{}, errInvalidRevision
	}
	return Revision{
		version:   target.Version(),
		branch:    target.Branch(),
		commit:    commit,
		sourceKey: source.Key(),
	}, nil
}

// Version 返回完整产品版本。
func (r Revision) Version() string {
	return r.version
}

// Branch 返回目标发布分支。
func (r Revision) Branch() string {
	return r.branch
}

// Commit 返回 40 位小写 Git Commit。
func (r Revision) Commit() string {
	return r.commit
}

// SourceKey 返回最终成功网络源的稳定 key。
func (r Revision) SourceKey() string {
	return r.sourceKey
}

func (r Revision) validate() error {
	target, err := ParseTarget(r.version)
	if err != nil ||
		target.Branch() != r.branch ||
		!validCommit(r.commit) ||
		!validRevisionSourceKey(r.sourceKey) {
		return errInvalidRevision
	}
	return nil
}

type remoteSnapshot struct {
	name      string
	fetchURLs []string
	mirror    bool
}

type repositorySnapshot struct {
	nonBare        bool
	remotes        []remoteSnapshot
	headSymbolic   bool
	headTarget     string
	commit         string
	shallow        []string
	tags           []string
	versionMode    filemode.FileMode
	versionPayload []byte
}

type repositoryIdentity struct {
	version   string
	branch    string
	commit    string
	sourceKey string
}

func repositoryIdentityFromSnapshot(snapshot repositorySnapshot) (repositoryIdentity, error) {
	if !snapshot.nonBare || len(snapshot.remotes) != 1 {
		return repositoryIdentity{}, errInvalidRepositoryID
	}
	remote := snapshot.remotes[0]
	if remote.name != "origin" || len(remote.fetchURLs) != 1 || remote.mirror {
		return repositoryIdentity{}, errInvalidRepositoryID
	}
	source, sourceAllowed := recoverySourceForURL(remote.fetchURLs[0])
	if !sourceAllowed ||
		!snapshot.headSymbolic || !validCommit(snapshot.commit) ||
		!slices.Contains(snapshot.shallow, snapshot.commit) ||
		len(snapshot.tags) != 0 || !snapshot.versionMode.IsRegular() {
		return repositoryIdentity{}, errInvalidRepositoryID
	}
	version, err := parseRepositoryVersion(snapshot.versionPayload)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("%w: version document: %w", errInvalidRepositoryID, err)
	}
	target, err := ParseTarget(version)
	if err != nil || snapshot.headTarget != plumbing.NewBranchReferenceName(target.Branch()).String() {
		return repositoryIdentity{}, errInvalidRepositoryID
	}
	return repositoryIdentity{
		version:   target.Version(),
		branch:    target.Branch(),
		commit:    snapshot.commit,
		sourceKey: source.Key(),
	}, nil
}

func recoverySourceForURL(value string) (mirror.Source, bool) {
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return mirror.Source{}, false
	}
	for _, source := range catalog.Sources(mirror.KindGit) {
		if source.BaseURL() == value {
			return source, true
		}
	}
	return mirror.Source{}, false
}

type repositoryReader interface {
	Inspect(ctx context.Context, repositoryPath string) (repositorySnapshot, error)
}

type goGitRepositoryReader struct{}

func (goGitRepositoryReader) Inspect(
	ctx context.Context,
	repositoryPath string,
) (repositorySnapshot, error) {
	if ctx == nil || repositoryPath == "" {
		return repositorySnapshot{}, errInvalidRepositoryRead
	}
	if err := ctx.Err(); err != nil {
		return repositorySnapshot{}, err
	}
	repository, err := git.PlainOpen(repositoryPath)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("open repository: %w", err)
	}
	snapshot := repositorySnapshot{}
	if _, err := repository.Worktree(); err != nil {
		if errors.Is(err, git.ErrIsBareRepository) {
			return snapshot, nil
		}
		return repositorySnapshot{}, fmt.Errorf("open repository worktree: %w", err)
	}
	snapshot.nonBare = true

	if err := ctx.Err(); err != nil {
		return repositorySnapshot{}, err
	}
	remotes, err := repository.Remotes()
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read repository remotes: %w", err)
	}
	snapshot.remotes = make([]remoteSnapshot, 0, len(remotes))
	for _, remote := range remotes {
		if remote == nil || remote.Config() == nil {
			return repositorySnapshot{}, errors.New("repository remote is invalid")
		}
		remoteConfig := remote.Config()
		snapshot.remotes = append(snapshot.remotes, remoteSnapshot{
			name:      remoteConfig.Name,
			fetchURLs: append([]string(nil), remoteConfig.URLs...),
			mirror:    remoteConfig.Mirror,
		})
	}

	if err := ctx.Err(); err != nil {
		return repositorySnapshot{}, err
	}
	head, err := repository.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read repository HEAD: %w", err)
	}
	var commitHash plumbing.Hash
	switch head.Type() {
	case plumbing.SymbolicReference:
		snapshot.headSymbolic = true
		snapshot.headTarget = head.Target().String()
		resolved, err := repository.Reference(head.Target(), true)
		if err != nil {
			return repositorySnapshot{}, fmt.Errorf("resolve repository HEAD: %w", err)
		}
		commitHash = resolved.Hash()
	case plumbing.HashReference:
		commitHash = head.Hash()
	default:
		return repositorySnapshot{}, errors.New("repository HEAD type is invalid")
	}
	snapshot.commit = commitHash.String()
	commit, err := repository.CommitObject(commitHash)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read repository HEAD commit: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return repositorySnapshot{}, err
	}
	shallow, err := repository.Storer.Shallow()
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read shallow boundary: %w", err)
	}
	snapshot.shallow = make([]string, 0, len(shallow))
	for _, hash := range shallow {
		snapshot.shallow = append(snapshot.shallow, hash.String())
	}

	tags, err := repository.Tags()
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read repository tags: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			tags.Close()
			return repositorySnapshot{}, err
		}
		reference, nextErr := tags.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			tags.Close()
			return repositorySnapshot{}, fmt.Errorf("iterate repository tags: %w", nextErr)
		}
		snapshot.tags = append(snapshot.tags, reference.Name().String())
	}
	tags.Close()

	versionFile, err := commit.File(repositoryVersionTreePath)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read repository version entry: %w", err)
	}
	snapshot.versionMode = versionFile.Mode
	if versionFile.Size < 0 || versionFile.Size > maxRepositoryVersionBytes {
		return repositorySnapshot{}, errVersionFileTooLarge
	}
	reader, err := versionFile.Reader()
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("open repository version blob: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, maxRepositoryVersionBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return repositorySnapshot{}, errors.Join(
			wrapRepositoryError("read repository version blob", readErr),
			wrapRepositoryError("close repository version blob", closeErr),
		)
	}
	if len(payload) > maxRepositoryVersionBytes {
		return repositorySnapshot{}, errVersionFileTooLarge
	}
	if err := ctx.Err(); err != nil {
		return repositorySnapshot{}, err
	}
	snapshot.versionPayload = payload
	return snapshot, nil
}

func wrapRepositoryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func validGitSource(source mirror.Source) bool {
	if source.Kind() != mirror.KindGit {
		return false
	}
	rebuilt, err := mirror.NewSource(
		source.Kind(),
		source.Key(),
		source.BaseURL(),
		source.Official(),
	)
	return err == nil && rebuilt == source
}

func validRevisionSourceKey(key string) bool {
	if key == "" {
		return false
	}
	previousHyphen := false
	for i := 0; i < len(key); i++ {
		character := key[i]
		switch {
		case character >= 'a' && character <= 'z':
			previousHyphen = false
		case character >= '0' && character <= '9':
			if i == 0 {
				return false
			}
			previousHyphen = false
		case character == '-':
			if i == 0 || i == len(key)-1 || previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return true
}

var _ repositoryReader = goGitRepositoryReader{}
