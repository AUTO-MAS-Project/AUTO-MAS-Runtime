package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const maxRestoredSourceBytes int64 = 64 << 20

type sourceRestore struct {
	path    string
	payload []byte
}

// Restore 以 Commit tree 为准恢复跟踪文件，所有目标 Blob 验证通过后才允许修改。
func (goGitRepositoryReader) Restore(ctx context.Context, layout *config.Layout, commit string, beforeWrite func() error) (int, error) {
	if ctx == nil || layout == nil || !validCommit(commit) || beforeWrite == nil {
		return 0, errInvalidRepositoryRead
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	repository, err := git.PlainOpen(layout.RepoDir())
	if err != nil {
		return 0, err
	}
	if err := verifySourceObject(ctx, repository, plumbing.CommitObject, plumbing.NewHash(commit)); err != nil {
		return 0, err
	}
	objectCommit, err := repository.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return 0, err
	}
	tree, err := objectCommit.Tree()
	if err != nil {
		return 0, err
	}
	if err := verifySourceTrees(ctx, repository, tree); err != nil {
		return 0, err
	}
	policy, err := newCheckoutPolicy(repositoryCheckoutExclusions())
	if err != nil {
		return 0, err
	}
	pending := make([]sourceRestore, 0)
	err = tree.Files().ForEach(func(file *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		root, _, _ := strings.Cut(file.Name, "/")
		if _, excluded := policy.exclusions[root]; excluded {
			return nil
		}
		if (file.Mode != filemode.Regular && file.Mode != filemode.Executable) || file.Size < 0 || file.Size > maxRestoredSourceBytes {
			return errors.New("tracked source has unsupported mode or size")
		}
		path, err := layout.RepositoryFile(file.Name)
		if err != nil {
			return err
		}
		actual, err := filesystem.ReadManagedFile(ctx, layout, path, file.Size+1)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && int64(len(actual)) == file.Size && plumbing.ComputeHash(plumbing.BlobObject, actual) == file.Hash {
			return nil
		}
		reader, err := file.Reader()
		if err != nil {
			return err
		}
		payload, readErr := io.ReadAll(io.LimitReader(reader, maxRestoredSourceBytes+1))
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return err
		}
		if int64(len(payload)) != file.Size || plumbing.ComputeHash(plumbing.BlobObject, payload) != file.Hash {
			return errors.New("tracked source blob hash does not match commit")
		}
		pending = append(pending, sourceRestore{path: path, payload: payload})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("inspect tracked source: %w", err)
	}
	head, err := repository.Head()
	if err != nil {
		return 0, err
	}
	if head.Hash().String() != commit {
		return 0, errors.New("repository commit changed during source inspection")
	}
	if len(pending) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := beforeWrite(); err != nil {
		return 0, err
	}
	for index, file := range pending {
		if err := filesystem.ReplaceManagedFile(ctx, layout, file.path, file.payload); err != nil {
			return index, err
		}
		actual, err := filesystem.ReadManagedFile(ctx, layout, file.path, int64(len(file.payload))+1)
		if err != nil {
			return index + 1, err
		}
		if plumbing.ComputeHash(plumbing.BlobObject, actual) != plumbing.ComputeHash(plumbing.BlobObject, file.payload) {
			return index + 1, errors.New("restored source verification failed")
		}
	}
	return len(pending), nil
}

func verifySourceTrees(ctx context.Context, repository *git.Repository, tree *object.Tree) error {
	if err := verifySourceObject(ctx, repository, plumbing.TreeObject, tree.Hash); err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		if entry.Mode != filemode.Dir {
			continue
		}
		subtree, err := tree.Tree(entry.Name)
		if err != nil {
			return err
		}
		if err := verifySourceTrees(ctx, repository, subtree); err != nil {
			return err
		}
	}
	return nil
}

func verifySourceObject(ctx context.Context, repository *git.Repository, kind plumbing.ObjectType, hash plumbing.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := repository.Storer.EncodedObject(kind, hash)
	if err != nil {
		return err
	}
	if encoded.Size() < 0 || encoded.Size() > maxRestoredSourceBytes {
		return errors.New("source object size is invalid")
	}
	reader, err := encoded.Reader()
	if err != nil {
		return err
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, maxRestoredSourceBytes+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return err
	}
	if int64(len(payload)) != encoded.Size() || plumbing.ComputeHash(kind, payload) != hash {
		return errors.New("source object hash does not match commit")
	}
	return ctx.Err()
}

func (s *Service) restoreOrFinishUnchanged(ctx context.Context, request SyncRequest, runtime syncRuntime, machine *protocol.LifecycleMachine, check CheckResult, environment state.EnvironmentState, logPath string) (result SyncResult, returnErr error) {
	started := false
	lease, err := filesystem.PinManagedDirectory(ctx, s.layout, s.layout.RepoDir())
	if err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			if started {
				returnErr = errors.Join(returnErr, serviceCommittedStateWriteError(protocol.StageWorkspaceCleanup, closeErr))
			} else {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	if check.directoryIdentity == nil || !check.directoryIdentity.Equal(lease.Identity()) {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, filesystem.ErrIdentityChanged)
	}
	count, err := s.reader.Restore(ctx, s.layout, check.Commit, func() error {
		broken, err := runtime.NewBrokenEnvironment(environment.LastSuccessful, state.BrokenEnvironment{TargetVersion: check.Version, Branch: check.Branch, Commit: check.Commit, Reason: state.ReasonRepositoryChanged, Stage: protocol.StageWorkspaceVerify, LogPath: logPath})
		if err != nil {
			return serviceStateWriteError(protocol.StageWorkspaceVerify, err)
		}
		if err := runtime.WriteEnvironment(ctx, broken); err != nil {
			return serviceStateWriteError(protocol.StageWorkspaceVerify, err)
		}
		started = true
		if err := machine.Transition(protocol.StateEnvironmentBroken); err != nil {
			return err
		}
		return emitState(request, protocol.StageWorkspaceVerify, protocol.StateEnvironmentBroken, "正在恢复受管程序文件", map[string]any{})
	})
	if err != nil {
		code := protocol.CodeWorkspaceRestoreFailed
		var coded interface{ Code() protocol.Code }
		if errors.As(err, &coded) {
			code = coded.Code()
		}
		if isCancellation(ctx, err) {
			code = protocol.CodeOperationCancelled
		}
		failure := newError(code, protocol.StageWorkspaceVerify, "受管程序文件恢复失败", map[string]any{"restoredFiles": count, "logPath": logPath}, err)
		failure.committed = started
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, failure)
	}
	if count == 0 {
		return s.finishUnchanged(ctx, request, runtime, machine, check)
	}
	if err := emitState(request, protocol.StageWorkspaceVerify, protocol.StateEnvironmentBroken, "受管程序文件已恢复，正在重新准备运行环境", map[string]any{"restoredFiles": count}); err != nil {
		return SyncResult{}, s.finishPreSwap(ctx, request, runtime, machine, serviceCommittedOperationError(err))
	}
	cleanupCtx, cancel := serviceCleanupContext(ctx)
	defer cancel()
	if err := removeTransaction(cleanupCtx, runtime, state.TransactionMutation); err != nil {
		return SyncResult{}, serviceCommittedStateWriteError(protocol.StageWorkspaceCleanup, err)
	}
	return SyncResult{Revision: Revision{version: check.Version, branch: check.Branch, commit: check.Commit, sourceKey: check.Source}, Changed: true, Status: protocol.StateEnvironmentBroken, ControlCommandID: controlCommandID(request)}, nil
}
