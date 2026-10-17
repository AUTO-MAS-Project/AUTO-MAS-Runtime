package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func inspectManagedFile(ctx context.Context, layout *config.Layout, path string) (FileInspection, error) {
	rootPath, relative, err := darwinManagedRelative(ctx, layout, path)
	if err != nil {
		return FileInspection{}, err
	}
	if relative == "." {
		return FileInspection{}, ErrIdentityChanged
	}
	root, err := openDarwinAbsoluteDirectory(ctx, rootPath.path)
	if isDarwinStableMissing(err) {
		return FileInspection{}, nil
	}
	if err != nil {
		return FileInspection{}, err
	}
	parent, openErr := root.openDirectory(ctx, filepath.Dir(relative), false)
	closeErr := root.file.Close()
	if openErr != nil || closeErr != nil {
		if parent != nil {
			closeErr = errors.Join(closeErr, parent.file.Close())
		}
		if isDarwinStableMissing(openErr) && closeErr == nil {
			return FileInspection{}, nil
		}
		return FileInspection{}, errors.Join(openErr, closeErr)
	}
	file, _, openErr := parent.openFile(ctx, filepath.Base(relative), os.O_RDONLY, 0)
	closeErr = parent.file.Close()
	if errors.Is(openErr, os.ErrNotExist) && closeErr == nil {
		return FileInspection{}, nil
	}
	if file != nil {
		closeErr = errors.Join(closeErr, file.Close())
	}
	if openErr != nil || closeErr != nil {
		return FileInspection{}, errors.Join(openErr, closeErr)
	}
	return FileInspection{Exists: true}, nil
}

func (d *darwinDirectory) openFile(ctx context.Context, name string, flags int, perm os.FileMode) (*os.File, DirectoryIdentity, error) {
	if ctx == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, DirectoryIdentity{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, DirectoryIdentity{}, err
	}
	var before unix.Stat_t
	beforeErr := unix.Fstatat(int(d.file.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW)
	if beforeErr == nil {
		if err := validateDarwinOrdinaryFile(before); err != nil {
			return nil, DirectoryIdentity{}, err
		}
	} else if !errors.Is(beforeErr, os.ErrNotExist) || flags&os.O_CREATE == 0 {
		return nil, DirectoryIdentity{}, beforeErr
	}
	// 延后截断，先确认打开的是单链接普通文件，避免对硬链接目标产生副作用。
	fd, err := unix.Openat(int(d.file.Fd()), name, (flags&^os.O_TRUNC)|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(perm.Perm()))
	if err != nil {
		return nil, DirectoryIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, DirectoryIdentity{}, errors.Join(err, file.Close())
	}
	if err := validateDarwinOrdinaryFile(stat); err != nil {
		return nil, DirectoryIdentity{}, errors.Join(err, file.Close())
	}
	identity := darwinIdentity(stat)
	if identity.volumeSerial != d.identity.volumeSerial {
		return nil, DirectoryIdentity{}, errors.Join(ErrIdentityChanged, file.Close())
	}
	if beforeErr == nil {
		previous := darwinIdentity(before)
		if !previous.Equal(&identity) {
			return nil, DirectoryIdentity{}, errors.Join(ErrIdentityChanged, file.Close())
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, DirectoryIdentity{}, errors.Join(err, file.Close())
	}
	if flags&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			return nil, DirectoryIdentity{}, errors.Join(err, file.Close())
		}
	}
	return file, identity, nil
}

func validateDarwinOrdinaryFile(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return unsafeDarwinLink("")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrIdentityChanged
	}
	if stat.Nlink != 1 {
		return ErrUnsafeHardLink
	}
	return nil
}
