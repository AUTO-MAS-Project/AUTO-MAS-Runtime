//go:build linux

package filesystem

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type pathAPI struct{}
type pinnedObject struct {
	chain    *posixChain
	file     *os.File
	path     string
	identity DirectoryIdentity
}

// CanonicalPath 保存 POSIX 词法路径，不折叠大小写或解析符号链接。
type CanonicalPath struct{ path string }

// DirectoryInspection 描述受管目录的存在性和设备/inode 身份。
type DirectoryInspection struct {
	Exists   bool
	Identity *DirectoryIdentity
}

// Canonicalize 只接受绝对路径；实际文件身份由打开能力确认。
func Canonicalize(path string) (CanonicalPath, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return CanonicalPath{}, ErrInvalidArgument
	}
	return CanonicalPath{path: config.SystemPath(filepath.Clean(path))}, nil
}
func (p CanonicalPath) String() string { return p.path }

// Native 返回本机路径。
func (p CanonicalPath) Native() string { return p.path }

// Equal 按路径组件比较，已存在的别名按文件身份比较。
func (p CanonicalPath) Equal(q CanonicalPath) bool {
	if p.path == "" || q.path == "" {
		return false
	}
	if p.path == q.path {
		return true
	}
	a, e := os.Lstat(p.path)
	b, f := os.Lstat(q.path)
	return e == nil && f == nil && a.Mode()&os.ModeSymlink == 0 && b.Mode()&os.ModeSymlink == 0 && os.SameFile(a, b)
}

// Contains 按完整组件边界判断严格后代。
func (p CanonicalPath) Contains(q CanonicalPath) bool {
	return p.path != "" && q.path != "" && p.path != q.path && within(p.path, q.path)
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type posixObjectIdentity struct {
	directory             DirectoryIdentity
	changeTimeSeconds     int64
	changeTimeNanoseconds int32
	generation            uint32
}

func posixIdentityFromStat(st *unix.Stat_t) posixObjectIdentity {
	id := DirectoryIdentity{volumeSerial: uint64(st.Dev), numberOfLinks: uint32(st.Nlink), size: st.Size, attributes: uint32(st.Mode)}
	binary.LittleEndian.PutUint64(id.fileID[:8], uint64(st.Ino))
	seconds, nanoseconds := posixStatChangeTime(st)
	return posixObjectIdentity{
		directory:             id,
		changeTimeSeconds:     seconds,
		changeTimeNanoseconds: nanoseconds,
		generation:            posixStatGeneration(st),
	}
}

func posixIdentity(file *os.File) (DirectoryIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return DirectoryIdentity{}, err
	}
	return posixIdentityFromStat(&st).directory, nil
}

// posixChain 同时保留所有祖先；每次变更前重新复核目录条目身份。
type posixChain struct {
	files []*os.File
	names []string
	ids   []DirectoryIdentity
}

func openPOSIXDirectory(ctx context.Context, path string, create bool) (*posixChain, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	canonical, err := Canonicalize(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	root := os.NewFile(uintptr(fd), "/")
	id, err := posixIdentity(root)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	c := &posixChain{files: []*os.File{root}, ids: []DirectoryIdentity{id}}
	for _, name := range strings.Split(strings.TrimPrefix(canonical.path, "/"), "/") {
		if name == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, c.close())
		}
		parent := int(c.leaf().Fd())
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(parent, name, 0700); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				err = &Error{code: protocol.CodeUnsafeReparsePoint, Operation: "open-directory", Path: path, Err: errors.Join(ErrIdentityChanged, err)}
			}
			return nil, errors.Join(err, c.close())
		}
		f := os.NewFile(uintptr(fd), name)
		id, err := posixIdentity(f)
		if err != nil {
			return nil, errors.Join(err, f.Close(), c.close())
		}
		c.files = append(c.files, f)
		c.ids = append(c.ids, id)
		c.names = append(c.names, name)
	}
	return c, nil
}
func (c *posixChain) leaf() *os.File { return c.files[len(c.files)-1] }
func (c *posixChain) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, name := range c.names {
		var st unix.Stat_t
		if err := unix.Fstatat(int(c.files[i].Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errors.Join(ErrIdentityChanged, err)
		}
		if uint64(st.Dev) != c.ids[i+1].volumeSerial || uint64(st.Ino) != binary.LittleEndian.Uint64(c.ids[i+1].fileID[:8]) || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrIdentityChanged
		}
	}
	return nil
}
func (c *posixChain) close() error {
	var err error
	for i := len(c.files) - 1; i >= 0; i-- {
		err = errors.Join(err, c.files[i].Close())
	}
	c.files = nil
	return err
}
func managedPOSIX(ctx context.Context, layout *config.Layout, path string, create bool) (*posixChain, error) {
	if ctx == nil || layout == nil || !filepath.IsAbs(path) || !within(layout.AppRoot(), filepath.Clean(path)) {
		return nil, ErrInvalidArgument
	}
	return openPOSIXDirectory(ctx, path, create)
}

// InspectManagedDirectory 拒绝目录链符号链接，并返回设备与 inode 身份。
func InspectManagedDirectory(ctx context.Context, layout *config.Layout, path string) (DirectoryInspection, error) {
	c, err := managedPOSIX(ctx, layout, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return DirectoryInspection{}, nil
	}
	if err != nil {
		return DirectoryInspection{}, err
	}
	id := c.ids[len(c.ids)-1]
	return DirectoryInspection{Exists: true, Identity: &id}, c.close()
}
func pinManagedDirectory(ctx context.Context, layout *config.Layout, path string) (*DirectoryLease, error) {
	c, err := managedPOSIX(ctx, layout, path, false)
	if err != nil {
		return nil, err
	}
	return newDirectoryLeaseWithIdentity(path, c.ids[len(c.ids)-1], c.close), nil
}
func prepareManagedDirectory(ctx context.Context, layout *config.Layout, path string) (*DirectoryLease, error) {
	if layout == nil || !within(layout.AppRoot(), path) || filepath.Clean(path) == layout.AppRoot() {
		return nil, ErrInvalidArgument
	}
	c, err := managedPOSIX(ctx, layout, filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	if err = c.verify(ctx); err == nil {
		err = unix.Mkdirat(int(c.leaf().Fd()), filepath.Base(path), 0700)
	}
	err = errors.Join(err, c.close())
	if err != nil {
		return nil, err
	}
	return pinManagedDirectory(ctx, layout, path)
}
