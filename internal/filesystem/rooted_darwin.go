package filesystem

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// RootedDirectory 只以现存目录租约授权相对操作，不通过路径重新打开根。
type RootedDirectory struct{ lease *DirectoryLease }

// RootedDirectory 让消费者在租约存活期间操作子树；消费者不接管租约。
func (l *DirectoryLease) RootedDirectory() (*RootedDirectory, error) {
	if l == nil || l.rootFile == nil || l.identity == nil {
		return nil, ErrInvalidArgument
	}
	return &RootedDirectory{lease: l}, nil
}

// CheckLocation 只复核名称仍指向已授权对象；复核得到的 FD 不用于子树操作。
func (r *RootedDirectory) CheckLocation(ctx context.Context) error {
	return r.withRoot(ctx, ".", func(root *darwinDirectory) error {
		current, err := openDarwinAbsoluteDirectory(ctx, root.path)
		if isDarwinStableMissing(err) {
			return errors.Join(ErrIdentityChanged, err)
		}
		if err != nil {
			return err
		}
		if !current.identity.Equal(&root.identity) {
			return errors.Join(ErrIdentityChanged, current.file.Close())
		}
		return current.file.Close()
	})
}

// OpenFile 打开不跟随链接的单链接普通文件。
func (r *RootedDirectory) OpenFile(ctx context.Context, name string, flags int, perm os.FileMode) (file *os.File, returnErr error) {
	returnErr = r.withParent(ctx, name, func(parent *darwinDirectory, leaf string) error {
		var err error
		file, _, err = parent.openFile(ctx, leaf, flags, perm)
		return err
	})
	if returnErr != nil && file != nil {
		return nil, errors.Join(returnErr, file.Close())
	}
	return file, returnErr
}

// Stat 仅描述同卷的普通文件或目录；不跟随叶子链接。
func (r *RootedDirectory) Stat(ctx context.Context, name string) (info os.FileInfo, returnErr error) {
	if name == "." {
		returnErr = r.withRoot(ctx, name, func(root *darwinDirectory) error {
			var err error
			info, err = root.file.Stat()
			return err
		})
		return info, returnErr
	}
	returnErr = r.withParent(ctx, name, func(parent *darwinDirectory, leaf string) error {
		var err error
		info, err = parent.rootedStat(ctx, leaf)
		return err
	})
	return info, returnErr
}

// ReadDir 从目录 FD 枚举并相对读取元数据，禁止通过 DirEntry.Info 回退到名称路径。
func (r *RootedDirectory) ReadDir(ctx context.Context, name string) (items []os.FileInfo, returnErr error) {
	returnErr = r.withRoot(ctx, name, func(root *darwinDirectory) (operationErr error) {
		dir, err := root.openDirectory(ctx, name, false)
		if err != nil {
			return err
		}
		defer func() { operationErr = joinOperationCleanup(operationErr, dir.file.Close()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			names, err := dir.file.Readdirnames(128)
			for _, leaf := range names {
				info, err := dir.rootedStat(ctx, leaf)
				if err != nil {
					return err
				}
				items = append(items, info)
			}
			if errors.Is(err, io.EOF) {
				slices.SortFunc(items, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
	return items, returnErr
}

// MkdirAll 逐级创建并重新验证目录，绝不通过路径跳过已存在的祖先。
func (r *RootedDirectory) MkdirAll(ctx context.Context, name string, perm os.FileMode) error {
	return r.withRoot(ctx, name, func(root *darwinDirectory) error {
		if name == "." {
			return nil
		}
		parent, err := root.openDirectory(ctx, ".", false)
		if err != nil {
			return err
		}
		for leaf := range strings.SplitSeq(name, "/") {
			if err := ctx.Err(); err != nil {
				return joinOperationCleanup(err, parent.file.Close())
			}
			if err := unix.Mkdirat(int(parent.file.Fd()), leaf, uint32(perm.Perm())); err != nil && !errors.Is(err, os.ErrExist) {
				return joinOperationCleanup(err, parent.file.Close())
			}
			child, openErr := parent.openDirectory(ctx, leaf, false)
			closeErr := parent.file.Close()
			if openErr != nil || closeErr != nil {
				if child != nil {
					closeErr = errors.Join(closeErr, child.file.Close())
				}
				return joinOperationCleanup(openErr, closeErr)
			}
			parent = child
		}
		return parent.file.Close()
	})
}

// Remove 仅移除验证过的普通文件或空目录，不递归。
func (r *RootedDirectory) Remove(ctx context.Context, name string) error {
	return r.withParent(ctx, name, func(parent *darwinDirectory, leaf string) error {
		identity, dir, err := parent.rootedIdentity(leaf)
		if err != nil {
			return err
		}
		if err := parent.verifyName(leaf, identity, dir); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		flags := 0
		if dir {
			flags = unix.AT_REMOVEDIR
		}
		return unix.Unlinkat(int(parent.file.Fd()), leaf, flags)
	})
}

// RenameFile 在同一授权根内重命名普通文件；仅允许替换已验证的普通文件。
func (r *RootedDirectory) RenameFile(ctx context.Context, from, to string) error {
	if !validDarwinRootedPath(to) || from == "." || to == "." {
		return ErrInvalidArgument
	}
	return r.withParent(ctx, from, func(src *darwinDirectory, oldLeaf string) (operationErr error) {
		oldID, dir, err := src.rootedIdentity(oldLeaf)
		if err != nil {
			return err
		}
		if dir {
			return ErrIdentityChanged
		}
		// 已持有根锁，目标父目录也只从同一个 FD 派生。
		dst, err := r.directory().openDirectory(ctx, path.Dir(to), false)
		if err != nil {
			return err
		}
		defer func() { operationErr = joinOperationCleanup(operationErr, dst.file.Close()) }()
		newLeaf := path.Base(to)
		flags := uint32(unix.RENAME_EXCL)
		newID, dir, err := dst.rootedIdentity(newLeaf)
		if err == nil {
			if dir {
				return ErrIdentityChanged
			}
			if err := dst.verifyName(newLeaf, newID, false); err != nil {
				return err
			}
			flags = 0
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := src.verifyName(oldLeaf, oldID, false); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return unix.RenameatxNp(int(src.file.Fd()), oldLeaf, int(dst.file.Fd()), newLeaf, flags)
	})
}

func (r *RootedDirectory) withRoot(ctx context.Context, name string, run func(*darwinDirectory) error) error {
	if ctx == nil || r == nil || r.lease == nil || !validDarwinRootedPath(name) {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.lease.mu.Lock()
	defer r.lease.mu.Unlock()
	if r.lease.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(r.directory())
}

func (r *RootedDirectory) directory() *darwinDirectory {
	l := r.lease
	return &darwinDirectory{file: l.rootFile, path: l.path, identity: *l.identity}
}

func (r *RootedDirectory) withParent(ctx context.Context, name string, run func(*darwinDirectory, string) error) error {
	if name == "." {
		return ErrInvalidArgument
	}
	return r.withRoot(ctx, name, func(root *darwinDirectory) error {
		dir, err := root.openDirectory(ctx, path.Dir(name), false)
		if err != nil {
			return err
		}
		return joinOperationCleanup(run(dir, path.Base(name)), dir.file.Close())
	})
}

func validDarwinRootedPath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00") && strings.Count(name, "/") < 256
}

func (d *darwinDirectory) rootedIdentity(leaf string) (DirectoryIdentity, bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(d.file.Fd()), leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return DirectoryIdentity{}, false, err
	}
	dir := stat.Mode&unix.S_IFMT == unix.S_IFDIR
	if !dir {
		if err := validateDarwinOrdinaryFile(stat); err != nil {
			return DirectoryIdentity{}, false, err
		}
	}
	identity := darwinIdentity(stat)
	if identity.volumeSerial != d.identity.volumeSerial {
		return DirectoryIdentity{}, false, ErrIdentityChanged
	}
	return identity, dir, nil
}

func (d *darwinDirectory) rootedStat(ctx context.Context, leaf string) (os.FileInfo, error) {
	identity, dir, err := d.rootedIdentity(leaf)
	if err != nil {
		return nil, err
	}
	var file *os.File
	if dir {
		child, err := d.openDirectory(ctx, leaf, false)
		if err != nil {
			return nil, err
		}
		if !child.identity.Equal(&identity) {
			return nil, errors.Join(ErrIdentityChanged, child.file.Close())
		}
		file = child.file
	} else {
		opened, actual, err := d.openFile(ctx, leaf, os.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		if !actual.Equal(&identity) {
			return nil, errors.Join(ErrIdentityChanged, opened.Close())
		}
		file = opened
	}
	info, statErr := file.Stat()
	return info, joinOperationCleanup(statErr, file.Close())
}
