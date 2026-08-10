//go:build !windows

package backend

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type unsupportedLockSet struct{}

func newLazyLockSet(*config.Layout) LockSet { return unsupportedLockSet{} }
func (unsupportedLockSet) Acquire(context.Context) (Lease, error) {
	return nil, errors.New("backend mutex is unsupported on this platform")
}
func (unsupportedLockSet) Close() error { return nil }
