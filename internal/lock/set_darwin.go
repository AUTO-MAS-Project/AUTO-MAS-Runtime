package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const darwinCoordinatorBudget = 2 * time.Second

// ProbeResult 描述一次协调门内的探测结果。
type ProbeResult struct {
	Held      bool
	Recovered bool
}

// AcquisitionResult 保存已签发租约与对端探测事实。
type AcquisitionResult struct {
	lease *Lease
	peer  ProbeResult
}

// Lease 持有一份独占业务文件描述符；关闭即释放 flock。
type Lease struct {
	set  *Set
	kind Kind
	name string

	// file 和 closeErr 由 set.serial 保护。
	file     *os.File
	closeErr error
}

// Set 固定 app root 及已观察的状态目录，每个实例最多签发一份活动租约。
type Set struct {
	// serial 允许等待实例内操作时响应调用方取消。
	serial chan struct{}

	appLease   *filesystem.DirectoryLease
	app        *filesystem.RootedDirectory
	stateLeaf  string
	backend    string
	mutation   string
	gate       string
	stateLease *filesystem.DirectoryLease
	state      *filesystem.RootedDirectory
	identities map[string]os.FileInfo
	active     *Lease
	poison     error
	closed     bool
	closeErr   error
}

// NewSet 只固定已存在的 app root，不为探测创建目录或锁文件。
func NewSet(ctx context.Context, layout *config.Layout) (*Set, error) {
	if ctx == nil || layout == nil {
		return nil, filesystem.ErrInvalidArgument
	}
	lease, err := filesystem.OpenManagedDirectory(ctx, layout, layout.AppRoot())
	if err != nil {
		return nil, darwinLockError("open-root", "", "", err)
	}
	root, err := lease.RootedDirectory()
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	stateLeaf, err := filepath.Rel(layout.AppRoot(), layout.StateDir())
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	paths := layout.DarwinLockPaths()
	return &Set{
		serial: make(chan struct{}, 1), appLease: lease, app: root, stateLeaf: stateLeaf,
		backend: filepath.Base(paths.Backend), mutation: filepath.Base(paths.Mutation),
		gate: filepath.Base(paths.Coordinator), identities: make(map[string]os.FileInfo, 3),
	}, nil
}

// Lease 返回本次成功获取签发的租约。
func (r AcquisitionResult) Lease() *Lease { return r.lease }

// Recovered 始终为 false，flock 释放不证明事务恢复。
func (AcquisitionResult) Recovered() bool { return false }

// PeerProbe 返回协调门内观察到的对端状态。
func (r AcquisitionResult) PeerProbe() ProbeResult { return r.peer }

// AcquireBackend 获取 backend，且拒绝已存在的 mutation。
func (s *Set) AcquireBackend(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindBackend, false)
}

// AcquireMutation 获取 mutation，且拒绝已存在的 backend。
func (s *Set) AcquireMutation(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindMutation, false)
}

// AcquireStaging 获取 mutation，允许另一个 Set 正持有 backend。
func (s *Set) AcquireStaging(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindMutation, true)
}

func (s *Set) acquire(ctx context.Context, kind Kind, staging bool) (AcquisitionResult, error) {
	var result AcquisitionResult
	if err := s.enter(ctx); err != nil {
		return result, err
	}
	defer s.leave()
	name := s.name(kind)
	var primary *os.File
	err := s.coordinated(ctx, true, func() error {
		if s.active != nil {
			if err := s.verifyActive(ctx); err != nil {
				return err
			}
			code := protocol.CodeMutationInProgress
			if s.active.kind == KindBackend {
				code = protocol.CodeBackendStillRunning
				if kind == KindBackend {
					code = protocol.CodeBackendAlreadyRunning
				}
			}
			return &ConflictError{code: code, Kind: s.active.kind, Name: s.active.name}
		}

		file, err := s.openKnown(ctx, name, true)
		if err != nil {
			return darwinLockError("open-primary", kind, name, err)
		}
		primary = file
		held, err := tryDarwinFlock(file)
		if err != nil {
			return darwinLockError("wait-primary", kind, name, err)
		}
		if held {
			code := protocol.CodeMutationInProgress
			if kind == KindBackend {
				code = protocol.CodeBackendAlreadyRunning
			}
			return &ConflictError{code: code, Kind: kind, Name: name}
		}

		peerKind := KindMutation
		if kind == KindMutation {
			peerKind = KindBackend
		}
		result.peer, err = s.probeKind(ctx, peerKind)
		if err != nil {
			return err
		}
		if result.peer.Held && !staging {
			code := protocol.CodeMutationInProgress
			if peerKind == KindBackend {
				code = protocol.CodeBackendStillRunning
			}
			return &ConflictError{code: code, Kind: peerKind, Name: s.name(peerKind)}
		}
		_, err = s.state.VerifyLockFile(ctx, name, file)
		return darwinLockError("verify-primary", kind, name, s.rememberIdentityFailure(err))
	})
	if err = errors.Join(err, ctx.Err()); err != nil {
		return result, errors.Join(err, s.closeFD(primary, "close-primary", kind, name))
	}
	result.lease = &Lease{set: s, kind: kind, name: name, file: primary}
	s.active = result.lease
	return result, nil
}

// Probe 在协调门内探测锁；不创建状态目录或任何叶。
func (s *Set) Probe(ctx context.Context, kind Kind) (ProbeResult, error) {
	if !kind.Valid() {
		return ProbeResult{}, fmt.Errorf("invalid mutex kind %q", kind)
	}
	if err := s.enter(ctx); err != nil {
		return ProbeResult{}, err
	}
	defer s.leave()
	var result ProbeResult
	err := s.coordinated(ctx, false, func() error {
		var err error
		result, err = s.probeKind(ctx, kind)
		return err
	})
	return result, errors.Join(err, ctx.Err())
}

func (s *Set) probeKind(ctx context.Context, kind Kind) (ProbeResult, error) {
	if s.active != nil && s.active.kind == kind {
		if err := s.verifyActive(ctx); err != nil {
			return ProbeResult{}, err
		}
		return ProbeResult{Held: true}, nil
	}
	name := s.name(kind)
	file, err := s.openKnown(ctx, name, false)
	if err != nil || file == nil {
		return ProbeResult{}, darwinLockError("open-probe", kind, name, err)
	}
	held, lockErr := tryDarwinFlock(file)
	_, identityErr := s.state.VerifyLockFile(ctx, name, file)
	err = errors.Join(darwinLockError("wait-probe", kind, name, lockErr),
		darwinLockError("verify-probe", kind, name, s.rememberIdentityFailure(identityErr)), s.closeFD(file, "close-probe", kind, name))
	return ProbeResult{Held: held}, err
}

func (s *Set) name(kind Kind) string {
	if kind == KindBackend {
		return s.backend
	}
	return s.mutation
}

func (s *Set) verifyActive(ctx context.Context) error {
	_, err := s.state.VerifyLockFile(ctx, s.active.name, s.active.file)
	return darwinLockError("verify-active", s.active.kind, s.active.name, s.rememberIdentityFailure(err))
}

func (s *Set) enter(ctx context.Context) error {
	if ctx == nil {
		return filesystem.ErrInvalidArgument
	}
	if s == nil || s.serial == nil {
		return ErrClosed
	}
	select {
	case s.serial <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	var err error
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case s.closed:
		err = ErrClosed
	case s.poison != nil:
		err = &PoisonedError{Cause: s.poison}
	}
	if err != nil {
		s.leave()
	}
	return err
}

func (s *Set) leave() { <-s.serial }

// Kind 返回租约的业务类型。
func (l *Lease) Kind() Kind { return l.kind }

// Name 返回不含安装根的稳定锁文件叶名。
func (l *Lease) Name() string { return l.name }

// Close 幂等地关闭当前租约的描述符，不删除锁文件。
func (l *Lease) Close() error {
	if l == nil || l.set == nil {
		return nil
	}
	l.set.serial <- struct{}{}
	defer l.set.leave()
	return l.set.closeLease(l)
}

func (s *Set) closeLease(lease *Lease) error {
	if lease.file != nil {
		lease.closeErr = s.closeFD(lease.file, "close-lease", lease.kind, lease.name)
		lease.file = nil
	}
	if s.active == lease {
		s.active = nil
	}
	return lease.closeErr
}

// Close 释放本实例的租约与目录 FD，重复调用返回同一结果。
func (s *Set) Close() error {
	if s == nil || s.serial == nil {
		return nil
	}
	s.serial <- struct{}{}
	defer s.leave()
	if !s.closed {
		s.closed = true
		var leaseErr error
		if s.active != nil {
			leaseErr = s.closeLease(s.active)
		}
		s.closeErr = errors.Join(leaseErr, s.stateLease.Close(), s.appLease.Close())
	}
	return s.closeErr
}

func (s *Set) closeFD(file *os.File, operation string, kind Kind, name string) error {
	if file == nil {
		return nil
	}
	err := darwinLockError(operation, kind, name, file.Close())
	if err != nil && s.poison == nil {
		s.poison = err
	}
	return err
}

func darwinLockError(operation string, kind Kind, name string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return &OperationError{Operation: operation, Kind: kind, Name: name, Cause: darwinLockCause{err: cause}}
}

type darwinLockCause struct{ err error }

func (e darwinLockCause) Error() string {
	var errno syscall.Errno
	switch {
	case errors.Is(e.err, filesystem.ErrIdentityChanged):
		return filesystem.ErrIdentityChanged.Error()
	case errors.Is(e.err, filesystem.ErrUnsafeHardLink):
		return filesystem.ErrUnsafeHardLink.Error()
	case errors.Is(e.err, errDarwinCoordinatorTimeout):
		return errDarwinCoordinatorTimeout.Error()
	case errors.As(e.err, &errno):
		return errno.Error()
	default:
		return "lock object could not be verified"
	}
}

func (e darwinLockCause) Unwrap() error { return e.err }
