//go:build !darwin

package gitrepo

import (
	"context"
	"errors"
	"os"

	git "github.com/go-git/go-git/v5"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func restoreFilesFor(_ *git.Repository, layout *config.Layout) (sourceRestoreFiles, error) {
	return sourceRestoreFiles{
		read: func(ctx context.Context, path string, limit int64) (sourceFileSnapshot, error) {
			data, err := filesystem.ReadManagedFile(ctx, layout, path, limit)
			if errors.Is(err, os.ErrNotExist) {
				return sourceFileSnapshot{}, nil
			}
			return sourceFileSnapshot{data: data, exists: err == nil}, err
		},
		replace: func(ctx context.Context, target sourceRestore) (filesystem.PublishResult, error) {
			err := filesystem.ReplaceManagedFile(ctx, layout, target.path, target.payload)
			return filesystem.PublishResult{Published: err == nil}, err
		},
	}, nil
}
