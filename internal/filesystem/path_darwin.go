package filesystem

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// CanonicalPath 保存 POSIX 词法路径和已验证的系统固定别名，不替代对象身份。
type CanonicalPath struct{ path string }

// DirectoryInspection 描述受管目录的只读存在性事实。
type DirectoryInspection struct {
	Exists   bool
	Identity *DirectoryIdentity
}

type darwinDirectory struct {
	file     *os.File
	path     string
	identity DirectoryIdentity
}

// Canonicalize 只解析系统固定别名；任意应用目录链接仍由 FD 遍历拒绝。
func Canonicalize(path string) (CanonicalPath, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return CanonicalPath{}, ErrInvalidArgument
	}
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return CanonicalPath{}, ErrInvalidArgument
		}
	}
	path = filepath.Clean(path)
	for _, alias := range []struct{ from, to string }{
		{from: "/tmp", to: "/private/tmp"},
		{from: "/var", to: "/private/var"},
	} {
		if path != alias.from && !strings.HasPrefix(path, alias.from+"/") {
			continue
		}
		info, err := os.Lstat(alias.from)
		if err != nil {
			return CanonicalPath{}, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(alias.from)
		if err != nil {
			return CanonicalPath{}, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(alias.from), target)
		}
		if filepath.Clean(target) != alias.to {
			return CanonicalPath{}, unsafeDarwinLink(alias.from)
		}
		path = alias.to + strings.TrimPrefix(path, alias.from)
		break
	}
	return CanonicalPath{path: path}, nil
}

// String 返回只供显示、分类与初步边界判断的绝对路径。
func (p CanonicalPath) String() string { return p.path }

// Native 返回 POSIX 路径；它本身不是后续 I/O 的授权。
func (p CanonicalPath) Native() string { return p.path }

// Equal 只比较规范词法值，物理同一性使用 DirectoryIdentity。
func (p CanonicalPath) Equal(other CanonicalPath) bool {
	return p.path != "" && p.path == other.path
}

// Contains 按组件边界判断词法后代，不将相邻前缀当成子目录。
func (p CanonicalPath) Contains(other CanonicalPath) bool {
	return p.path != "" && other.path != "" && other.path != p.path &&
		strings.HasPrefix(other.path, strings.TrimSuffix(p.path, "/")+"/")
}

func darwinManagedRelative(ctx context.Context, layout *config.Layout, path string) (CanonicalPath, string, error) {
	if ctx == nil || layout == nil {
		return CanonicalPath{}, "", ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return CanonicalPath{}, "", err
	}
	root, err := Canonicalize(layout.AppRoot())
	if err != nil {
		return CanonicalPath{}, "", err
	}
	target, err := Canonicalize(path)
	if err != nil {
		return CanonicalPath{}, "", err
	}
	if root.path == "/" || !root.Equal(target) && !root.Contains(target) {
		return CanonicalPath{}, "", &Error{code: protocol.CodePathOutsideManagedRoot,
			Operation: "managed-path", Path: path, Err: ErrIdentityChanged}
	}
	relative, err := filepath.Rel(root.path, target.path)
	return root, relative, err
}

func openDarwinAbsoluteDirectory(ctx context.Context, path string) (*darwinDirectory, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	canonical, err := Canonicalize(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "/")
	file, err = walkDarwinDirectories(ctx, file, strings.TrimPrefix(canonical.path, "/"), false, 0)
	if err != nil {
		return nil, err
	}
	return checkedDarwinDirectory(file, canonical.path, 0)
}

// walkDarwinDirectories 接管 start 的所有权，每步只接受单个 no-follow 目录组件。
func walkDarwinDirectories(ctx context.Context, start *os.File, relative string, createLeaf bool, expectedVolume uint64) (*os.File, error) {
	current := start
	if expectedVolume != 0 {
		var stat unix.Stat_t
		if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
			return nil, joinOperationCleanup(err, current.Close())
		}
		if uint64(uint32(stat.Dev)) != expectedVolume {
			return nil, joinOperationCleanup(ErrIdentityChanged, current.Close())
		}
	}
	if relative == "" || relative == "." {
		if err := ctx.Err(); err != nil {
			return nil, joinOperationCleanup(err, current.Close())
		}
		return current, nil
	}
	components := strings.Split(relative, "/")
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, joinOperationCleanup(ErrInvalidArgument, current.Close())
		}
		if err := ctx.Err(); err != nil {
			return nil, joinOperationCleanup(err, current.Close())
		}
		if createLeaf && index == len(components)-1 {
			if err := unix.Mkdirat(int(current.Fd()), component, 0o700); err != nil {
				return nil, joinOperationCleanup(err, current.Close())
			}
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(current.Fd()), component, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, joinOperationCleanup(err, current.Close())
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, joinOperationCleanup(unsafeDarwinLink(component), current.Close())
		}
		if expectedVolume != 0 && uint64(uint32(stat.Dev)) != expectedVolume {
			return nil, joinOperationCleanup(ErrIdentityChanged, current.Close())
		}
		fd, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, joinOperationCleanup(err, current.Close())
		}
		next := os.NewFile(uintptr(fd), component)
		var opened unix.Stat_t
		if err := unix.Fstat(fd, &opened); err != nil {
			return nil, joinOperationCleanup(err, errors.Join(next.Close(), current.Close()))
		}
		if opened.Dev != stat.Dev || opened.Ino != stat.Ino {
			return nil, joinOperationCleanup(ErrIdentityChanged, errors.Join(next.Close(), current.Close()))
		}
		if err := current.Close(); err != nil {
			return nil, errors.Join(err, next.Close())
		}
		current = next
	}
	return current, nil
}

func checkedDarwinDirectory(file *os.File, path string, expectedVolume uint64) (*darwinDirectory, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	identity := darwinIdentity(stat)
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || expectedVolume != 0 && identity.volumeSerial != expectedVolume {
		return nil, errors.Join(ErrIdentityChanged, file.Close())
	}
	var volume unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &volume); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if unix.ByteSliceToString(volume.Fstypename[:]) != "apfs" || volume.Flags&unix.MNT_LOCAL == 0 {
		return nil, errors.Join(fmt.Errorf("%w: only local APFS is validated", ErrUnsupported), file.Close())
	}
	return &darwinDirectory{file: file, path: path, identity: identity}, nil
}

func (d *darwinDirectory) openDirectory(ctx context.Context, relative string, createLeaf bool) (*darwinDirectory, error) {
	fd, err := unix.Openat(int(d.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file, err := walkDarwinDirectories(ctx, os.NewFile(uintptr(fd), d.path), relative, createLeaf, d.identity.volumeSerial)
	if err != nil {
		return nil, err
	}
	return checkedDarwinDirectory(file, filepath.Join(d.path, relative), d.identity.volumeSerial)
}

func darwinIdentity(stat unix.Stat_t) DirectoryIdentity {
	var fileID [16]byte
	binary.LittleEndian.PutUint64(fileID[:8], stat.Ino)
	return DirectoryIdentity{volumeSerial: uint64(uint32(stat.Dev)), fileID: fileID,
		attributes: uint32(stat.Mode), numberOfLinks: uint32(stat.Nlink), size: stat.Size}
}

func unsafeDarwinLink(path string) error {
	return &Error{code: protocol.CodeUnsafeReparsePoint, Operation: "symlink", Path: path,
		Err: errors.New("filesystem path contains a symbolic link")}
}

func isDarwinStableMissing(err error) bool {
	cause, cleanup := splitOperationCleanup(err)
	return cleanup == nil && errors.Is(cause, os.ErrNotExist)
}

func darwinOutside(operation, path string) error {
	return &Error{code: protocol.CodePathOutsideManagedRoot, Operation: operation, Path: path, Err: ErrIdentityChanged}
}
