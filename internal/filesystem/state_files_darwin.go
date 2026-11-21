//go:build darwin

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

// StateFiles 用固定的 state 目录句柄协调状态读写与条件删除。
//
// 目录在构造时被固定并记录物理身份；每次事务重新打开 state 目录、
// 复核身份后锁目录 inode，使跨进程事务落在同一物理对象上。
type StateFiles struct {
	mu       sync.Mutex // 保护 closed 与本能力的句柄生命周期。
	layout   *config.Layout
	identity DirectoryIdentity
	owner    *stateFileOwner
	closed   bool
	trace    func(string) // 仅供同包测试采集单次操作的锁时间线，由 mu 保护。
}

// NewStateFiles 准备 state 目录并固定其物理身份，不跟随任何目录链接。
func NewStateFiles(ctx context.Context, layout *config.Layout) (*StateFiles, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	directory, err := darwinStateDirectory(ctx, layout, true)
	if err != nil {
		return nil, err
	}
	identity := directory.identity
	if err := directory.file.Close(); err != nil {
		return nil, err
	}
	return &StateFiles{layout: layout, identity: identity, owner: &stateFileOwner{}}, nil
}

// darwinStateDirectory 按组件逐级打开 state 目录；create 只补建缺失的叶子目录。
//
// state 目录允许多个 Runtime 实例并发初始化，因此创建冲突收敛为 EEXIST，
// 之后一律重新按 no-follow 打开并复核身份；目录句柄能力的通用“准备目录”
// 语义（已存在即拒绝）保持不变。
func darwinStateDirectory(ctx context.Context, layout *config.Layout, create bool) (*darwinDirectory, error) {
	rootPath, relative, err := darwinManagedRelative(ctx, layout, layout.StateDir())
	if err != nil {
		return nil, err
	}
	root, err := openDarwinAbsoluteDirectory(ctx, rootPath.path)
	if err != nil {
		return nil, err
	}
	directory, openErr := root.openDirectory(ctx, relative, false)
	if create && isDarwinStableMissing(openErr) {
		if err := createDarwinStateLeaf(ctx, root, relative); err != nil {
			openErr = err
		} else {
			directory, openErr = root.openDirectory(ctx, relative, false)
		}
	}
	closeErr := root.file.Close()
	if openErr != nil || closeErr != nil {
		if directory != nil {
			closeErr = errors.Join(closeErr, directory.file.Close())
		}
		return nil, joinOperationCleanup(openErr, closeErr)
	}
	return directory, nil
}

// createDarwinStateLeaf 补建 state 目录叶子；并发创建由 EEXIST 收敛。
func createDarwinStateLeaf(ctx context.Context, root *darwinDirectory, relative string) error {
	if relative == "" || relative == "." {
		return ErrInvalidArgument
	}
	parent, err := root.openDirectory(ctx, filepath.Dir(relative), false)
	if err != nil {
		return err
	}
	mkdirErr := unix.Mkdirat(int(parent.file.Fd()), filepath.Base(relative), 0o700)
	closeErr := parent.file.Close()
	if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
		return errors.Join(mkdirErr, closeErr)
	}
	return closeErr
}

func (f *StateFiles) open(ctx context.Context, kind StateFileKind) (*darwinDirectory, string, error) {
	if f.closed {
		return nil, "", ErrClosed
	}
	if !kind.Valid() {
		return nil, "", ErrInvalidArgument
	}
	directory, err := darwinStateDirectory(ctx, f.layout, false)
	if err != nil {
		return nil, "", err
	}
	if !f.identity.Equal(&directory.identity) {
		return nil, "", errors.Join(ErrIdentityChanged, directory.file.Close())
	}
	// 锁目录 inode 而非可替换的守卫文件，跨进程事务始终落在同一物理对象。
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	f.traceState("wait")
	turn := darwinStateTurns.enqueue(darwinStateQueueKey{volume: directory.identity.volumeSerial, fileID: directory.identity.fileID})
	if err := darwinStateTurns.wait(waitCtx, turn); err != nil {
		f.traceState("wait-failed")
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = errors.Join(ErrStateGuardTimeout, err)
		}
		return nil, "", errors.Join(err, directory.file.Close())
	}
	directory.stateTurn = turn
	if err := lockDarwinStateDirectory(waitCtx, ctx, directory); err != nil {
		f.traceState("wait-failed")
		darwinStateTurns.release(turn)
		return nil, "", errors.Join(err, directory.file.Close())
	}
	f.traceState("acquired")
	if err := verifyDarwinStateDirectory(directory, f.identity); err != nil {
		closeErr := closeDarwinStateDirectory(directory)
		f.traceState("released")
		return nil, "", errors.Join(err, closeErr)
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
	return directory, filepath.Base(path), nil
}

func (f *StateFiles) traceState(event string) {
	if f.trace != nil {
		f.trace(event)
	}
}

// lockDarwinStateDirectory 在预算内独占目录 inode，超时映射为守卫占用。
func lockDarwinStateDirectory(waitCtx, requestCtx context.Context, directory *darwinDirectory) error {
	for {
		if err := waitCtx.Err(); err != nil {
			if requestCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return errors.Join(ErrStateGuardTimeout, err)
			}
			return err
		}
		err := unix.Flock(int(directory.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		if err := defaultWait(waitCtx, 10*time.Millisecond); err != nil {
			if requestCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return errors.Join(ErrStateGuardTimeout, err)
			}
			return err
		}
	}
}

// closeDarwinStateDirectory 释放目录锁并关闭句柄。
func closeDarwinStateDirectory(directory *darwinDirectory) error {
	err := errors.Join(unix.Flock(int(directory.file.Fd()), unix.LOCK_UN), directory.file.Close())
	if directory.stateTurn != nil {
		darwinStateTurns.release(directory.stateTurn)
		directory.stateTurn = nil
	}
	return err
}

// verifyDarwinStateDirectory 复核目录句柄仍指向构造时固定的对象。
func verifyDarwinStateDirectory(directory *darwinDirectory, expected DirectoryIdentity) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrIdentityChanged
	}
	actual := darwinIdentity(stat)
	if !expected.Equal(&actual) || actual.volumeSerial != directory.identity.volumeSerial {
		return ErrIdentityChanged
	}
	return nil
}

func (f *StateFiles) readDarwin(
	ctx context.Context,
	directory *darwinDirectory,
	name string,
	kind StateFileKind,
	limit int64,
) (StateFileSnapshot, error) {
	file, identity, err := directory.openFile(ctx, name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return StateFileSnapshot{}, errors.Join(ErrStateFileNotFound, err)
	}
	if err != nil {
		return StateFileSnapshot{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		err = ErrStateFileTooLarge
	}
	if err == nil {
		// 读取后复核名称仍指向同一物理对象，拒绝读取期间的对象替换。
		_, err = directory.verifyFile(name, file, identity)
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return StateFileSnapshot{}, err
	}
	return StateFileSnapshot{
		owner:        f.owner,
		kind:         kind,
		volumeSerial: identity.volumeSerial,
		fileID:       identity.fileID,
		size:         int64(len(data)),
		digest:       sha256.Sum256(data),
		bytes:        data,
	}, nil
}

// Read 在事务锁内读取有界快照。
func (f *StateFiles) Read(ctx context.Context, kind StateFileKind, limit int64) (StateFileSnapshot, error) {
	if f == nil || ctx == nil || limit <= 0 || limit > MaxStateFileBytes {
		return StateFileSnapshot{}, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	directory, name, err := f.open(ctx, kind)
	if err != nil {
		return StateFileSnapshot{}, err
	}
	snapshot, readErr := f.readDarwin(ctx, directory, name, kind, limit)
	return snapshot, errors.Join(readErr, closeDarwinStateDirectory(directory))
}

// WriteAtomic 由同目录独占临时文件、文件同步、rename 与目录同步组成提交点。
func (f *StateFiles) WriteAtomic(ctx context.Context, kind StateFileKind, data []byte) (WriteAtomicResult, error) {
	if f == nil || ctx == nil || len(data) == 0 || int64(len(data)) > MaxStateFileBytes {
		return WriteAtomicResult{}, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	directory, name, err := f.open(ctx, kind)
	if err != nil {
		return WriteAtomicResult{}, err
	}
	applied, writeErr := replaceDarwin(ctx, directory, name, data)
	f.traceState("write-finished")
	closeErr := closeDarwinStateDirectory(directory)
	f.traceState("released")
	err = errors.Join(writeErr, closeErr)
	result := WriteAtomicResult{MutationApplied: applied, RecoveryRequired: applied && err != nil}
	if err != nil {
		return result, &StateWriteError{
			Phase:            StateWritePhaseWrite,
			MutationApplied:  applied,
			RecoveryRequired: result.RecoveryRequired,
			Cause:            err,
		}
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
	directory, name, err := f.open(ctx, snapshot.kind)
	if err != nil {
		return StateRemoveResult{}, err
	}
	actual, err := f.readDarwin(ctx, directory, name, snapshot.kind, MaxStateFileBytes)
	if errors.Is(err, ErrStateFileNotFound) {
		return StateRemoveResult{}, closeDarwinStateDirectory(directory)
	}
	if err == nil && (actual.volumeSerial != snapshot.volumeSerial || actual.fileID != snapshot.fileID || actual.digest != snapshot.digest) {
		err = ErrIdentityChanged
	}
	if err == nil {
		err = unix.Unlinkat(int(directory.file.Fd()), name, 0)
	}
	applied := err == nil
	if applied {
		err = directory.file.Sync()
	}
	err = errors.Join(err, closeDarwinStateDirectory(directory))
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
