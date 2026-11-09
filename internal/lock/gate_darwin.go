package lock

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

var errDarwinCoordinatorTimeout = errors.New("flock coordinator wait budget exceeded")

func (s *Set) coordinated(ctx context.Context, create bool, run func() error) (returnErr error) {
	if err := s.prepareState(ctx, create); err != nil {
		return darwinLockError("open-lock-directory", "", "", err)
	}
	if s.state == nil {
		return nil
	}
	gate, err := s.openCoordinator(ctx, create)
	if err != nil {
		return darwinLockError("open-coordinator", "", s.gate, err)
	}
	if gate == nil {
		return darwinLockError("verify-lock-directory", "", s.gate, s.checkLocations(ctx))
	}
	defer func() {
		returnErr = errors.Join(returnErr, s.closeFD(gate, "close-coordinator", "", s.gate))
	}()
	if err := waitDarwinCoordinator(ctx, gate); err != nil {
		return darwinLockError("wait-coordinator", "", s.gate, err)
	}
	if err := s.verifyGate(ctx, gate); err != nil {
		return err
	}
	operationErr := run()
	return errors.Join(operationErr, s.verifyGate(ctx, gate))
}

func (s *Set) prepareState(ctx context.Context, create bool) error {
	if err := s.app.CheckLocation(ctx); err != nil {
		return s.rememberDirectoryFailure(err)
	}
	if s.state != nil {
		return s.rememberDirectoryFailure(s.state.CheckLocation(ctx))
	}
	if create {
		if err := s.app.MkdirAll(ctx, s.stateLeaf, 0o700); err != nil {
			return err
		}
	}
	lease, err := s.app.OpenChildDirectory(ctx, s.stateLeaf)
	if err != nil {
		return err
	}
	if lease == nil {
		if create {
			return s.rememberIdentityFailure(filesystem.ErrIdentityChanged)
		}
		return nil
	}
	root, err := lease.RootedDirectory()
	if err != nil {
		return errors.Join(err, lease.Close())
	}
	s.stateLease, s.state = lease, root
	return s.checkLocations(ctx)
}

func (s *Set) checkLocations(ctx context.Context) error {
	if err := s.app.CheckLocation(ctx); err != nil {
		return s.rememberDirectoryFailure(err)
	}
	return s.rememberDirectoryFailure(s.state.CheckLocation(ctx))
}

func (s *Set) openKnown(ctx context.Context, name string, create bool) (*os.File, error) {
	expected := s.identities[name]
	// 已知叶缺失时不重建，否则旧持有者与新名字会落在两个 flock 对象。
	file, err := s.state.OpenLockFile(ctx, name, create && expected == nil)
	if err != nil {
		if expected != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, s.rememberIdentityFailure(errors.Join(filesystem.ErrIdentityChanged, err))
		}
		return nil, err
	}
	if file == nil {
		if expected != nil {
			return nil, s.rememberIdentityFailure(filesystem.ErrIdentityChanged)
		}
		return nil, nil
	}
	id, err := s.state.VerifyLockFile(ctx, name, file)
	if err == nil && expected != nil && !os.SameFile(expected, id) {
		err = filesystem.ErrIdentityChanged
	}
	if err != nil {
		return nil, errors.Join(s.rememberIdentityFailure(err), s.closeFD(file, "close-unverified", "", name))
	}
	s.identities[name] = id
	return file, nil
}

func (s *Set) verifyGate(ctx context.Context, gate *os.File) error {
	if err := s.checkLocations(ctx); err != nil {
		return darwinLockError("verify-lock-directory", "", s.gate, err)
	}
	_, err := s.state.VerifyLockFile(ctx, s.gate, gate)
	return darwinLockError("verify-coordinator", "", s.gate, s.rememberIdentityFailure(err))
}

func (s *Set) openCoordinator(ctx context.Context, create bool) (*os.File, error) {
	gate, err := s.openKnown(ctx, s.gate, false)
	if err != nil || gate != nil {
		return gate, err
	}
	for _, name := range []string{s.backend, s.mutation} {
		file, err := s.openKnown(ctx, name, false)
		if err != nil {
			return nil, err
		}
		if file == nil {
			continue
		}
		if err := s.closeFD(file, "close-uncoordinated", "", name); err != nil {
			return nil, err
		}
		// 另一个实例可能已完成首次初始化。此时只打开其门，绝不补建未知现场的门。
		gate, err := s.openKnown(ctx, s.gate, false)
		if err != nil || gate != nil {
			return gate, err
		}
		return nil, s.rememberIdentityFailure(filesystem.ErrIdentityChanged)
	}
	if !create {
		return nil, nil
	}
	return s.openKnown(ctx, s.gate, true)
}

func (s *Set) rememberDirectoryFailure(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return s.rememberIdentityFailure(errors.Join(filesystem.ErrIdentityChanged, err))
}

func (s *Set) rememberIdentityFailure(err error) error {
	if errors.Is(err, filesystem.ErrIdentityChanged) && s.poison == nil {
		s.poison = darwinLockError("verify-identity", "", "", err)
	}
	return err
}

func tryDarwinFlock(file *os.File) (held bool, err error) {
	if file == nil {
		return false, filesystem.ErrIdentityChanged
	}
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}

func waitDarwinCoordinator(ctx context.Context, file *os.File) error {
	budget := time.NewTimer(darwinCoordinatorBudget)
	defer budget.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, err := tryDarwinFlock(file)
		if err != nil || !held {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-budget.C:
			return errDarwinCoordinatorTimeout
		case <-tick.C:
		}
	}
}
