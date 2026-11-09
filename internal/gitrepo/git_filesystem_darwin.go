package gitrepo

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	git "github.com/go-git/go-git/v5"
	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

const invalidGitPath = "\x00"

type darwinGitFS struct {
	owner  *darwinGitOwner
	prefix string
}

type darwinGitOwner struct {
	root     *filesystem.RootedDirectory
	rootName string
	run      func(func(context.Context) error) error

	mu     sync.Mutex // 保护作用域状态及 go-git 留下的文件，关闭后不再允许路径操作。
	closed bool
	files  map[*darwinGitFile]struct{}
}

type darwinGitFile struct {
	*os.File
	owner *darwinGitOwner
	name  string
}

func newDarwinGitFS(ctx context.Context, root *filesystem.RootedDirectory, rootName string) *darwinGitFS {
	owner := &darwinGitOwner{root: root, rootName: rootName, files: make(map[*darwinGitFile]struct{})}
	// billy 没有 ctx 参数；此回调只在本次同步 Git 操作内有效。
	owner.run = func(operation func(context.Context) error) error {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.closed {
			return filesystem.ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return operation(ctx)
	}
	return &darwinGitFS{owner: owner, prefix: "."}
}

func (f *darwinGitFS) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o666)
}

func (f *darwinGitFS) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

func (f *darwinGitFS) OpenFile(name string, flags int, perm os.FileMode) (file billy.File, returnErr error) {
	returnErr = f.owner.run(func(ctx context.Context) error {
		full, err := f.resolve(ctx, name)
		if err != nil {
			return err
		}
		if flags&os.O_CREATE != 0 {
			if err := f.owner.root.MkdirAll(ctx, path.Dir(full), 0o755); err != nil {
				return err
			}
		}
		opened, err := f.owner.root.OpenFile(ctx, full, flags, perm)
		if err != nil {
			return err
		}
		owned := &darwinGitFile{File: opened, name: name, owner: f.owner}
		f.owner.files[owned] = struct{}{}
		file = owned
		return nil
	})
	return file, returnErr
}

func (f *darwinGitFS) Stat(name string) (info os.FileInfo, returnErr error) {
	if name == "" {
		name = "."
	}
	returnErr = f.withPath(name, func(ctx context.Context, full string) error {
		var err error
		info, err = f.owner.root.Stat(ctx, full)
		return err
	})
	return info, returnErr
}

func (f *darwinGitFS) Lstat(name string) (os.FileInfo, error) { return f.Stat(name) }

func (f *darwinGitFS) Rename(from, to string) error {
	return f.withPath(from, func(ctx context.Context, src string) error {
		dst, err := f.resolve(ctx, to)
		if err != nil || from == "." || to == "." {
			return errors.Join(billy.ErrCrossedBoundary, err)
		}
		info, err := f.owner.root.Stat(ctx, src)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return filesystem.ErrIdentityChanged
		}
		if err := f.owner.root.MkdirAll(ctx, path.Dir(dst), 0o755); err != nil {
			return err
		}
		return f.owner.root.RenameFile(ctx, src, dst)
	})
}

func (f *darwinGitFS) Remove(name string) error {
	if name == "." {
		return billy.ErrCrossedBoundary
	}
	return f.withPath(name, f.owner.root.Remove)
}

func (f *darwinGitFS) Join(parts ...string) string {
	for _, part := range parts {
		if part != "" && !validGitPath(part) {
			// Join 无错误返回值；保留不可访问的标记，不能先 Clean 消除越界证据。
			return invalidGitPath
		}
	}
	return path.Join(parts...)
}

func (f *darwinGitFS) TempFile(dir, prefix string) (billy.File, error) {
	if dir == "" {
		dir = "."
	}
	if strings.ContainsAny(prefix, "/\\\x00") {
		return nil, billy.ErrCrossedBoundary
	}
	for range 16 {
		name := f.Join(dir, prefix+rand.Text())
		file, err := f.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			return file, err
		}
	}
	return nil, fs.ErrExist
}

func (f *darwinGitFS) ReadDir(name string) (items []os.FileInfo, returnErr error) {
	if name == "" {
		name = "."
	}
	returnErr = f.withPath(name, func(ctx context.Context, full string) error {
		var err error
		items, err = f.owner.root.ReadDir(ctx, full)
		return err
	})
	return items, returnErr
}

func (f *darwinGitFS) MkdirAll(name string, perm os.FileMode) error {
	if name == "" {
		name = "."
	}
	return f.withPath(name, func(ctx context.Context, full string) error {
		return f.owner.root.MkdirAll(ctx, full, perm)
	})
}

func (f *darwinGitFS) Chmod(name string, mode os.FileMode) error {
	return f.withPath(name, func(ctx context.Context, full string) error {
		file, err := f.owner.root.OpenFile(ctx, full, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		return errors.Join(file.Chmod(mode.Perm()), file.Close())
	})
}

func (*darwinGitFS) Symlink(_, _ string) error         { return billy.ErrNotSupported }
func (*darwinGitFS) Readlink(_ string) (string, error) { return "", billy.ErrNotSupported }

func (f *darwinGitFS) Chroot(name string) (billy.Filesystem, error) {
	if name == "" {
		name = "."
	}
	var child *darwinGitFS
	err := f.withPath(name, func(ctx context.Context, full string) error {
		info, err := f.owner.root.Stat(ctx, full)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && !info.IsDir() {
			return filesystem.ErrIdentityChanged
		}
		child = &darwinGitFS{owner: f.owner, prefix: full}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return child, nil
}

func (f *darwinGitFS) Root() string { return path.Join(f.owner.rootName, f.prefix) }

func (f *darwinGitFS) withPath(name string, operation func(context.Context, string) error) error {
	return f.owner.run(func(ctx context.Context) error {
		full, err := f.resolve(ctx, name)
		if err != nil {
			return err
		}
		return operation(ctx, full)
	})
}

func (f *darwinGitFS) resolve(ctx context.Context, name string) (string, error) {
	if !validGitPath(name) {
		return "", billy.ErrCrossedBoundary
	}
	full := path.Join(f.prefix, name)
	if f.prefix == git.GitDirName || strings.HasPrefix(f.prefix, git.GitDirName+"/") {
		return full, nil
	}
	first, _, _ := strings.Cut(full, "/")
	if strings.EqualFold(first, git.GitDirName) {
		return "", billy.ErrCrossedBoundary
	}
	if first != "." && !isASCII(first) {
		// APFS 可能忽略某些 Unicode 字符；用对象身份拒绝实际指向 .git 的别名。
		meta, err := f.owner.root.Stat(ctx, git.GitDirName)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err == nil {
			entry, err := f.owner.root.Stat(ctx, first)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			if err == nil && os.SameFile(meta, entry) {
				return "", billy.ErrCrossedBoundary
			}
		}
	}
	return full, nil
}

func validGitPath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00")
}

func isASCII(name string) bool {
	for index := range len(name) {
		if name[index] >= 0x80 {
			return false
		}
	}
	return true
}

func (o *darwinGitOwner) close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	var result error
	for file := range o.files {
		result = errors.Join(result, file.File.Close())
		delete(o.files, file)
	}
	return result
}

func (f *darwinGitFile) Name() string { return f.name }

func (f *darwinGitFile) Close() error {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if _, open := f.owner.files[f]; !open {
		return nil
	}
	delete(f.owner.files, f)
	return f.File.Close()
}

func (f *darwinGitFile) Lock() error   { return f.flock(unix.LOCK_EX | unix.LOCK_NB) }
func (f *darwinGitFile) Unlock() error { return f.flock(unix.LOCK_UN) }

func (f *darwinGitFile) flock(mode int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	err = conn.Control(func(fd uintptr) { lockErr = unix.Flock(int(fd), mode) })
	return errors.Join(err, lockErr)
}

var (
	_ billy.Filesystem = (*darwinGitFS)(nil)
	_ billy.Chmod      = (*darwinGitFS)(nil)
	_ billy.File       = (*darwinGitFile)(nil)
)
