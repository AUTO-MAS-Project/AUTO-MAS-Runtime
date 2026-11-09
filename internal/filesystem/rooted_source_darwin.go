package filesystem

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// ReadSource 返回受管源码的内容及身份；只有确定缺失时才返回空身份且无错误。
func (r *RootedDirectory) ReadSource(ctx context.Context, name string, limit int64) ([]byte, *DirectoryIdentity, error) {
	if limit < 0 {
		return nil, nil, ErrInvalidArgument
	}
	var payload []byte
	var expected *DirectoryIdentity
	err := r.withParent(ctx, name, func(parent *darwinDirectory, leaf string) (returnErr error) {
		file, id, err := parent.openFile(ctx, leaf, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer func() { returnErr = joinOperationCleanup(returnErr, file.Close()) }()
		payload, err = io.ReadAll(io.LimitReader(file, limit))
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := parent.verifyFile(leaf, file, id); err != nil {
			return errors.Join(ErrIdentityChanged, err)
		}
		expected = &id
		return nil
	})
	if isDarwinStableMissing(err) && !errors.Is(err, ErrIdentityChanged) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return payload, expected, nil
}

// ReplaceSource 原子替换预检时的同一对象，或在确定缺失的名称独占发布。
func (r *RootedDirectory) ReplaceSource(ctx context.Context, name string, data []byte, perm os.FileMode, expected *DirectoryIdentity) (result PublishResult, returnErr error) {
	if name == "." || !validDarwinRootedPath(name) {
		return result, ErrInvalidArgument
	}
	if err := r.CheckLocation(ctx); err != nil {
		return result, err
	}
	if expected == nil {
		if err := r.MkdirAll(ctx, path.Dir(name), 0o755); err != nil {
			return result, err
		}
	}
	returnErr = r.withParent(ctx, name, func(parent *darwinDirectory, leaf string) error {
		var err error
		result, err = parent.replaceDarwinSource(ctx, leaf, data, perm, expected)
		return err
	})
	return result, returnErr
}

func (d *darwinDirectory) replaceDarwinSource(ctx context.Context, leaf string, data []byte, perm os.FileMode, expected *DirectoryIdentity) (result PublishResult, returnErr error) {
	flags := uint32(unix.RENAME_EXCL)
	previous, oldID, err := d.openFile(ctx, leaf, os.O_RDONLY, 0)
	if previous != nil {
		defer func() { returnErr = joinOperationCleanup(returnErr, previous.Close()) }()
	}
	if expected != nil {
		if err != nil || !oldID.Equal(expected) {
			return result, errors.Join(ErrIdentityChanged, err)
		}
		flags = 0
	} else if !isDarwinStableMissing(err) {
		return result, errors.Join(ErrIdentityChanged, err)
	}

	partName := ".runtime-restore-" + rand.Text()
	part, partID, err := d.openFile(ctx, partName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, err
	}
	defer func() {
		var cleanupErr error
		if !result.Published {
			cleanupErr = d.removeFile(partName, part, partID)
		}
		returnErr = joinOperationCleanup(returnErr, errors.Join(cleanupErr, part.Close()))
	}()
	if _, err := part.Write(data); err != nil {
		return result, err
	}
	if err := part.Chmod(perm.Perm()); err != nil {
		return result, err
	}
	if err := part.Sync(); err != nil {
		return result, err
	}
	if _, err := d.verifyFile(partName, part, partID); err != nil {
		return result, err
	}
	if previous != nil {
		if _, err := d.verifyFile(leaf, previous, oldID); err != nil {
			return result, errors.Join(ErrIdentityChanged, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	fd := int(d.file.Fd())
	if err := unix.RenameatxNp(fd, partName, fd, leaf, flags); err != nil {
		if errors.Is(err, os.ErrExist) {
			return result, errors.Join(ErrIdentityChanged, err)
		}
		return result, err
	}
	result.Published = true
	return result, errors.Join(d.verifyName(leaf, partID, false), d.file.Sync())
}
