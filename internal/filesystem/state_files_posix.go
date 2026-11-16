//go:build linux

package filesystem

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// StateFiles 用固定目录句柄协调状态读写与条件删除。
type StateFiles struct {
	mu       sync.Mutex // 保护 closed 与本能力的句柄生命周期。
	layout   *config.Layout
	identity *DirectoryIdentity
	owner    *stateFileOwner
	closed   bool
}

// NewStateFiles 准备状态目录，不跟随任何目录链接。
func NewStateFiles(ctx context.Context, layout *config.Layout) (*StateFiles, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	c, err := managedPOSIX(ctx, layout, layout.StateDir(), true)
	if err != nil {
		return nil, err
	}
	id := c.ids[len(c.ids)-1]
	if err = c.close(); err != nil {
		return nil, err
	}
	return &StateFiles{layout: layout, identity: &id, owner: &stateFileOwner{}}, nil
}
func (f *StateFiles) open(ctx context.Context, kind StateFileKind) (*posixChain, string, error) {
	if f.closed {
		return nil, "", ErrClosed
	}
	if !kind.Valid() {
		return nil, "", ErrInvalidArgument
	}
	c, err := managedPOSIX(ctx, f.layout, f.layout.StateDir(), false)
	if err != nil {
		return nil, "", err
	}
	if !f.identity.Equal(&c.ids[len(c.ids)-1]) {
		return nil, "", errors.Join(ErrIdentityChanged, c.close())
	}
	// 锁目录 inode 而非可替换的守卫文件，跨进程事务始终落在同一物理对象。
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		err = unix.Flock(int(c.leaf().Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, "", errors.Join(err, c.close())
		}
		if err = defaultWait(waitCtx, 10*time.Millisecond); err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				err = errors.Join(ErrStateGuardTimeout, err)
			}
			return nil, "", errors.Join(err, c.close())
		}
	}
	if err = c.verify(ctx); err != nil {
		return nil, "", errors.Join(err, c.close())
	}
	var path string
	switch kind {
	case StateBackend:
		path = f.layout.BackendStateFile()
	case StateMutation:
		path = f.layout.MutationStateFile()
	case StateUpdate:
		path = f.layout.UpdateStateFile()
	case StateEnvironment:
		path = f.layout.EnvironmentStateFile()
	}
	return c, filepath.Base(path), nil
}
func (f *StateFiles) read(ctx context.Context, c *posixChain, name string, kind StateFileKind, limit int64) (StateFileSnapshot, error) {
	file, err := openPOSIXFile(c, name, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return StateFileSnapshot{}, errors.Join(ErrStateFileNotFound, err)
	}
	if err != nil {
		return StateFileSnapshot{}, err
	}
	id, err := posixIdentity(file)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(file, limit+1))
	}
	if int64(len(data)) > limit {
		err = ErrStateFileTooLarge
	}
	err = errors.Join(err, c.verify(ctx), file.Close())
	if err != nil {
		return StateFileSnapshot{}, err
	}
	return StateFileSnapshot{owner: f.owner, kind: kind, volumeSerial: id.volumeSerial, fileID: id.fileID, size: int64(len(data)), digest: sha256.Sum256(data), bytes: data}, nil
}

// Read 在事务锁内读取有界快照。
func (f *StateFiles) Read(ctx context.Context, kind StateFileKind, limit int64) (StateFileSnapshot, error) {
	if f == nil || ctx == nil || limit <= 0 || limit > MaxStateFileBytes {
		return StateFileSnapshot{}, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, name, err := f.open(ctx, kind)
	if err != nil {
		return StateFileSnapshot{}, err
	}
	snapshot, err := f.read(ctx, c, name, kind, limit)
	return snapshot, errors.Join(err, c.close())
}

// WriteAtomic 同目录独占临时文件、文件同步、rename 与父目录同步组成提交点。
func (f *StateFiles) WriteAtomic(ctx context.Context, kind StateFileKind, data []byte) (WriteAtomicResult, error) {
	if f == nil || ctx == nil || len(data) == 0 || int64(len(data)) > MaxStateFileBytes {
		return WriteAtomicResult{}, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, name, err := f.open(ctx, kind)
	if err != nil {
		return WriteAtomicResult{}, err
	}
	applied, err := replacePOSIX(ctx, c, name, data)
	err = errors.Join(err, c.close())
	result := WriteAtomicResult{MutationApplied: applied, RecoveryRequired: applied && err != nil}
	if err != nil {
		return result, &StateWriteError{Phase: StateWritePhaseWrite, MutationApplied: applied, RecoveryRequired: result.RecoveryRequired, Cause: err}
	}
	return result, nil
}

// RemoveTransactionIfUnchanged 仅允许移除本能力读取的未变化事务快照。
func (f *StateFiles) RemoveTransactionIfUnchanged(ctx context.Context, snapshot StateFileSnapshot) (StateRemoveResult, error) {
	if f == nil || ctx == nil || snapshot.owner != f.owner || (!snapshot.kind.Valid() || snapshot.kind == StateEnvironment) {
		return StateRemoveResult{}, ErrInvalidToken
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, name, err := f.open(ctx, snapshot.kind)
	if err != nil {
		return StateRemoveResult{}, err
	}
	actual, err := f.read(ctx, c, name, snapshot.kind, MaxStateFileBytes)
	if err == nil && (actual.volumeSerial != snapshot.volumeSerial || actual.fileID != snapshot.fileID || actual.digest != snapshot.digest) {
		err = ErrIdentityChanged
	}
	if err == nil {
		err = c.verify(ctx)
	}
	if err == nil {
		err = unix.Unlinkat(int(c.leaf().Fd()), name, 0)
	}
	applied := err == nil
	if applied {
		err = c.leaf().Sync()
	}
	err = errors.Join(err, c.close())
	return StateRemoveResult{MutationApplied: applied, RecoveryRequired: applied && err != nil}, err
}

// Close 幂等停用状态能力。
func (f *StateFiles) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
