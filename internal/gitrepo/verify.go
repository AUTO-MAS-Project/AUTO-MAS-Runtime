package gitrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var (
	errInvalidVerifier        = errors.New("repository verifier is invalid")
	errInvalidVersionDocument = errors.New("repository version document is invalid")
)

// VerificationRequest 固定静态校验所需的路径、目标和本次网络源集合。
type VerificationRequest struct {
	RepositoryPath string
	Target         Target
	Source         mirror.Source
	AllowedSources []mirror.Source
}

// Verifier 校验克隆仓库的来源、分支、对象边界和版本文档。
type Verifier struct {
	reader repositoryReader
}

// NewVerifier 创建使用 go-git 本地对象读取器的静态验证器。
func NewVerifier() *Verifier {
	return &Verifier{reader: goGitRepositoryReader{}}
}

func newVerifierWithReader(reader repositoryReader) (*Verifier, error) {
	if nilDependency(reader) {
		return nil, errInvalidVerifier
	}
	return &Verifier{reader: reader}, nil
}

// Verify 返回与可变仓库对象隔离的 Revision。
func (v *Verifier) Verify(
	ctx context.Context,
	request VerificationRequest,
) (Revision, error) {
	if ctx == nil || v == nil || nilDependency(v.reader) {
		return Revision{}, newError(
			protocol.CodeInternalError,
			protocol.StageWorkspaceVerify,
			messageForCode(protocol.CodeInternalError),
			map[string]any{},
			errInvalidVerifier,
		)
	}
	if err := request.Target.validate(); err != nil {
		return Revision{}, newError(
			protocol.CodeInvalidVersion,
			protocol.StageWorkspaceVerify,
			messageForCode(protocol.CodeInvalidVersion),
			map[string]any{},
			err,
		)
	}
	if reason := invalidVerificationRequest(request); reason != "" {
		return Revision{}, invalidRepositoryError(reason, errInvalidVerifier)
	}
	if err := ctx.Err(); err != nil {
		return Revision{}, verificationCancelledError(err)
	}

	snapshot, err := v.reader.Inspect(ctx, request.RepositoryPath)
	if ctxErr := ctx.Err(); ctxErr != nil || isCancellation(ctx, err) {
		return Revision{}, verificationCancelledError(errors.Join(ctxErr, err))
	}
	if err != nil {
		return Revision{}, invalidRepositoryError("repository_unreadable", err)
	}
	if reason := invalidSnapshotReason(snapshot, request); reason != "" {
		return Revision{}, invalidRepositoryError(reason, errInvalidVerifier)
	}
	version, err := parseRepositoryVersion(snapshot.versionPayload)
	if err != nil {
		return Revision{}, invalidRepositoryError("version_invalid", err)
	}
	if version != request.Target.Version() {
		return Revision{}, newError(
			protocol.CodeGitVersionMismatch,
			protocol.StageWorkspaceVerify,
			messageForCode(protocol.CodeGitVersionMismatch),
			map[string]any{"reason": "version_mismatch"},
			errors.New("repository version differs from target"),
		)
	}
	revision, err := newRevision(request.Target, snapshot.commit, request.Source)
	if err != nil {
		return Revision{}, invalidRepositoryError("revision_invalid", err)
	}
	return revision, nil
}

func invalidVerificationRequest(request VerificationRequest) string {
	if request.RepositoryPath == "" || !validGitSource(request.Source) || len(request.AllowedSources) == 0 {
		return "request_invalid"
	}
	seenKeys := make(map[string]struct{}, len(request.AllowedSources))
	seenURLs := make(map[string]struct{}, len(request.AllowedSources))
	sourceAllowed := false
	for _, source := range request.AllowedSources {
		if !validGitSource(source) {
			return "source_invalid"
		}
		if _, exists := seenKeys[source.Key()]; exists {
			return "source_duplicate"
		}
		if _, exists := seenURLs[source.BaseURL()]; exists {
			return "source_duplicate"
		}
		seenKeys[source.Key()] = struct{}{}
		seenURLs[source.BaseURL()] = struct{}{}
		if source == request.Source {
			sourceAllowed = true
		}
	}
	if !sourceAllowed {
		return "source_unknown"
	}
	return ""
}

func invalidSnapshotReason(snapshot repositorySnapshot, request VerificationRequest) string {
	if !snapshot.nonBare {
		return "repository_bare"
	}
	if len(snapshot.remotes) != 1 {
		return "remote_count"
	}
	remote := snapshot.remotes[0]
	if remote.name != "origin" || len(remote.fetchURLs) != 1 || remote.mirror {
		return "remote_shape"
	}
	if remote.fetchURLs[0] != request.Source.BaseURL() {
		return "remote_unknown"
	}
	if !snapshot.headSymbolic ||
		snapshot.headTarget != plumbing.NewBranchReferenceName(request.Target.Branch()).String() {
		return "branch_invalid"
	}
	if !validCommit(snapshot.commit) {
		return "commit_invalid"
	}
	if !slices.Contains(snapshot.shallow, snapshot.commit) {
		return "shallow_invalid"
	}
	if len(snapshot.tags) != 0 {
		return "tags_present"
	}
	if !snapshot.versionMode.IsRegular() {
		return "version_file_mode"
	}
	return ""
}

func parseRepositoryVersion(payload []byte) (string, error) {
	if len(payload) == 0 || len(payload) > maxRepositoryVersionBytes || !utf8.Valid(payload) {
		return "", errInvalidVersionDocument
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return "", errInvalidVersionDocument
	}
	seenVersion := false
	version := ""
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", errInvalidVersionDocument
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", errInvalidVersionDocument
		}
		if key == "version" {
			if seenVersion {
				return "", errInvalidVersionDocument
			}
			seenVersion = true
			var value any
			if err := decoder.Decode(&value); err != nil {
				return "", errInvalidVersionDocument
			}
			var ok bool
			version, ok = value.(string)
			if !ok {
				return "", errInvalidVersionDocument
			}
			continue
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return "", errInvalidVersionDocument
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || !seenVersion {
		return "", errInvalidVersionDocument
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errInvalidVersionDocument
	}
	return version, nil
}

func invalidRepositoryError(reason string, cause error) *Error {
	return newError(
		protocol.CodeGitRepositoryInvalid,
		protocol.StageWorkspaceVerify,
		messageForCode(protocol.CodeGitRepositoryInvalid),
		map[string]any{"reason": reason},
		cause,
	)
}

func verificationCancelledError(cause error) *Error {
	return newError(
		protocol.CodeOperationCancelled,
		protocol.StageWorkspaceVerify,
		messageForCode(protocol.CodeOperationCancelled),
		map[string]any{},
		cause,
	)
}

var _ cloneVerifier = (*Verifier)(nil)
