package gitrepo

import (
	"context"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func (*serviceTestReader) Restore(context.Context, *config.Layout, string, func() error) (int, error) {
	return 0, nil
}
func (*blockingServiceReader) Restore(context.Context, *config.Layout, string, func() error) (int, error) {
	return 0, nil
}
func (*fakeRepositoryReader) Restore(context.Context, *config.Layout, string, func() error) (int, error) {
	return 0, nil
}
