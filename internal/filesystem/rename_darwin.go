package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// AtomicRename 以共用分类规则授权，再执行同卷 no-replace 原子目录移动。
func (o *Operator) AtomicRename(ctx context.Context, request RenameRequest) (RenameResult, error) {
	if o == nil || o.layout == nil {
		return RenameResult{}, ErrInvalidArgument
	}
	if err := validateRenameRequestValues(ctx, request); err != nil {
		return RenameResult{}, err
	}
	expectedSource, expectedDestination, err := o.expectedRenamePaths(request)
	if err != nil {
		return RenameResult{}, err
	}
	for _, pair := range [][2]string{{expectedSource, request.Source}, {expectedDestination, request.Destination}} {
		expected, err := Canonicalize(pair[0])
		if err != nil {
			return RenameResult{}, err
		}
		actual, err := Canonicalize(pair[1])
		if err != nil {
			return RenameResult{}, err
		}
		if !expected.Equal(actual) {
			return RenameResult{}, darwinOutside("authorize-rename", actual.path)
		}
	}
	sourceParent, err := o.openDarwinDirectory(ctx, filepath.Dir(request.Source))
	if err != nil {
		return RenameResult{}, err
	}
	destinationParent, err := o.openDarwinDirectory(ctx, filepath.Dir(request.Destination))
	if err != nil {
		return RenameResult{}, errors.Join(err, sourceParent.file.Close())
	}
	source, err := sourceParent.openDirectory(ctx, filepath.Base(request.Source), false)
	if err != nil {
		return RenameResult{}, errors.Join(err, sourceParent.file.Close(), destinationParent.file.Close())
	}
	result, renameErr := o.renameDarwinPinned(ctx, request, sourceParent, source, destinationParent)
	return result, errors.Join(renameErr, source.file.Close(), sourceParent.file.Close(), destinationParent.file.Close())
}

func (o *Operator) renameDarwinPinned(ctx context.Context, request RenameRequest, sourceParent, source, destinationParent *darwinDirectory) (RenameResult, error) {
	if source.identity.volumeSerial != destinationParent.identity.volumeSerial ||
		request.ExpectedSourceIdentity != nil && !request.ExpectedSourceIdentity.Equal(&source.identity) {
		return RenameResult{}, ErrIdentityChanged
	}
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return RenameResult{}, err
		}
		if err := sourceParent.verifyName(filepath.Base(request.Source), source.identity, true); err != nil {
			return RenameResult{}, err
		}
		err := unix.RenameatxNp(int(sourceParent.file.Fd()), filepath.Base(request.Source),
			int(destinationParent.file.Fd()), filepath.Base(request.Destination), unix.RENAME_EXCL)
		if err == nil {
			return RenameResult{MutationApplied: true}, errors.Join(
				destinationParent.verifyName(filepath.Base(request.Destination), source.identity, true),
				sourceParent.file.Sync(), destinationParent.file.Sync())
		}
		if errors.Is(err, os.ErrExist) {
			return RenameResult{}, occupiedRenameError(request.Destination, errors.Join(ErrDestinationExists, err))
		}
		if !errors.Is(err, unix.EBUSY) {
			return RenameResult{}, err
		}
		if index >= len(o.delays) {
			return RenameResult{}, occupiedRenameError(request.Destination, err)
		}
		if err := o.wait(ctx, o.delays[index]); err != nil {
			return RenameResult{}, err
		}
	}
}
