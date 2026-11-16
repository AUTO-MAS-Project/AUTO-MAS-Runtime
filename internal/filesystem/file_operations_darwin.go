package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func openDarwinManagedDirectory(ctx context.Context, layout *config.Layout, path string) (*darwinDirectory, error) {
	lease, err := EnsureManagedDirectory(ctx, layout, path)
	if err != nil {
		return nil, err
	}
	lease.mu.Lock()
	directory := &darwinDirectory{file: lease.rootFile, path: lease.path, identity: *lease.identity}
	copy, openErr := directory.openDirectory(ctx, ".", false)
	lease.mu.Unlock()
	closeErr := lease.Close()
	if openErr != nil || closeErr != nil {
		if copy != nil {
			closeErr = errors.Join(closeErr, copy.file.Close())
		}
		return nil, joinOperationCleanup(openErr, closeErr)
	}
	return copy, nil
}

func withDarwinLock(ctx context.Context, file *os.File, mode int, operation func() error) error {
	if ctx == nil || file == nil || operation == nil {
		return ErrInvalidArgument
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if err := defaultWait(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	return joinOperationCleanup(operation(), unix.Flock(int(file.Fd()), unix.LOCK_UN))
}

func darwinPinnedDirectory(object pinnedObject) *darwinDirectory {
	return &darwinDirectory{file: object.file, path: object.file.Name(), identity: object.identity}
}

func darwinPin(directory *darwinDirectory) pinnedObject {
	return pinnedObject{file: directory.file, identity: directory.identity}
}

func closeDarwinPins(objects []pinnedObject) error {
	var result error
	for index := len(objects) - 1; index >= 0; index-- {
		if objects[index].file != nil {
			result = errors.Join(result, objects[index].file.Close())
			objects[index].file = nil
		}
	}
	return result
}

func (d *darwinDirectory) verifyFile(name string, file *os.File, expected DirectoryIdentity) (DirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return DirectoryIdentity{}, err
	}
	if err := validateDarwinOrdinaryFile(stat); err != nil {
		return DirectoryIdentity{}, err
	}
	actual := darwinIdentity(stat)
	if !expected.Equal(&actual) || actual.volumeSerial != d.identity.volumeSerial {
		return DirectoryIdentity{}, ErrIdentityChanged
	}
	if err := d.verifyName(name, expected, false); err != nil {
		return DirectoryIdentity{}, err
	}
	return actual, nil
}

func (d *darwinDirectory) verifyName(name string, expected DirectoryIdentity, directory bool) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return ErrInvalidArgument
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(d.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if directory {
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrIdentityChanged
		}
	} else if err := validateDarwinOrdinaryFile(stat); err != nil {
		return err
	}
	actual := darwinIdentity(stat)
	if !expected.Equal(&actual) || actual.volumeSerial != d.identity.volumeSerial {
		return ErrIdentityChanged
	}
	return nil
}

func (d *darwinDirectory) removeFile(name string, file *os.File, expected DirectoryIdentity) error {
	if _, err := d.verifyFile(name, file, expected); err != nil {
		return err
	}
	return unix.Unlinkat(int(d.file.Fd()), name, 0)
}
