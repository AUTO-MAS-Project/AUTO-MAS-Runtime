//go:build windows

package backend

import (
	"context"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
)

type lazyLockSet struct {
	mu     sync.Mutex
	layout *config.Layout
	set    *lock.Set
	closed bool
}

func newLazyLockSet(layout *config.Layout) LockSet {
	return &lazyLockSet{layout: layout}
}

func (s *lazyLockSet) Acquire(ctx context.Context) (Lease, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, lock.ErrClosed
	}
	if s.set == nil {
		set, err := lock.NewSet(ctx, s.layout)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.set = set
	}
	set := s.set
	s.mu.Unlock()
	result, err := set.AcquireBackend(ctx)
	if err != nil {
		return nil, err
	}
	lease := result.Lease()
	if lease == nil {
		return nil, lock.ErrClosed
	}
	return lease, nil
}

func (s *lazyLockSet) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.set == nil {
		return nil
	}
	return s.set.Close()
}
