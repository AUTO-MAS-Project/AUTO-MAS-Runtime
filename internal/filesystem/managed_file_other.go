//go:build !windows

package filesystem

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

var errManagedFileUnsupported = errors.New("managed file inspection is unsupported")

func inspectManagedFile(
	context.Context,
	*config.Layout,
	string,
) (FileInspection, error) {
	return FileInspection{}, errManagedFileUnsupported
}
