//go:build !windows

package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

var errManagedDirectoryUnsupported = errors.New("managed directory lease is unsupported")

func prepareManagedDirectory(
	context.Context,
	*config.Layout,
	string,
) (*DirectoryLease, error) {
	return nil, errManagedDirectoryUnsupported
}

func pinManagedDirectory(
	context.Context,
	*config.Layout,
	string,
) (*DirectoryLease, error) {
	return nil, errManagedDirectoryUnsupported
}
