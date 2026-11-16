//go:build linux

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

// NewRuntimeLogFiles 固定受管日志目录身份。
func NewRuntimeLogFiles(ctx context.Context, layout *config.Layout) (*RuntimeLogFiles, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	c, err := managedPOSIX(ctx, layout, layout.RuntimeLogDir(), true)
	if err != nil {
		return nil, err
	}
	f := &RuntimeLogFiles{layout: layout, owner: &runtimeLogOwner{}}
	f.pins[0].chain = c
	return f, nil
}

// OpenAppend 打开当前首卷日志。
func (f *RuntimeLogFiles) OpenAppend(ctx context.Context, command string, date time.Time) (*RuntimeLogWriter, error) {
	return f.OpenAppendSegment(ctx, command, date, 0)
}

// OpenAppendSegment 追加已有卷或创建首个卷。
func (f *RuntimeLogFiles) OpenAppendSegment(ctx context.Context, command string, date time.Time, sequence uint64) (*RuntimeLogWriter, error) {
	return f.openSegment(ctx, command, date, sequence, false)
}

// CreateSegment 独占创建新的日志卷。
func (f *RuntimeLogFiles) CreateSegment(ctx context.Context, command string, date time.Time, sequence uint64) (*RuntimeLogWriter, error) {
	return f.openSegment(ctx, command, date, sequence, true)
}
func (f *RuntimeLogFiles) openSegment(ctx context.Context, command string, date time.Time, sequence uint64, exclusive bool) (*RuntimeLogWriter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	path, err := f.layout.RuntimeLogSegmentFile(command, date, sequence)
	if err != nil {
		return nil, err
	}
	if err = f.pins[0].chain.verify(ctx); err != nil {
		return nil, err
	}
	c, err := managedPOSIX(ctx, f.layout, f.layout.RuntimeLogDir(), false)
	if err != nil {
		return nil, err
	}
	flags := unix.O_WRONLY | unix.O_APPEND | unix.O_CREAT
	if exclusive {
		flags |= unix.O_EXCL
	}
	file, err := openPOSIXFile(c, filepath.Base(path), flags)
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	w := &RuntimeLogWriter{path: path, file: pinnedObject{file: file}}
	w.pins[0].chain = c
	return w, nil
}

// Write 追加一条完整记录，容量不足时拒绝整条记录。
func (w *RuntimeLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if err := w.pins[0].chain.verify(context.Background()); err != nil {
		return 0, err
	}
	stat, err := w.file.file.Stat()
	if err != nil {
		return 0, err
	}
	if stat.Size()+int64(len(data)) > MaxRuntimeLogBytes {
		return 0, ErrRuntimeLogCapacity
	}
	return w.file.file.Write(data)
}

// Path 返回日志诊断路径。
func (w *RuntimeLogWriter) Path() string { return w.path }

// Close 幂等释放写入句柄。
func (w *RuntimeLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		w.closeErr = errors.Join(w.file.file.Close(), w.pins[0].chain.close())
	}
	return w.closeErr
}

// List 返回已检查的普通日志文件身份令牌。
func (f *RuntimeLogFiles) List(ctx context.Context) ([]RuntimeLogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	c := f.pins[0].chain
	if err := c.verify(ctx); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(c.leaf().Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), f.layout.RuntimeLogDir())
	names, err := directory.Readdirnames(-1)
	err = errors.Join(err, directory.Close())
	if err != nil {
		return nil, err
	}
	result := make([]RuntimeLogFile, 0, len(names))
	for _, name := range names {
		file, err := openPOSIXFile(c, name, unix.O_RDONLY)
		if err != nil {
			return nil, err
		}
		var st unix.Stat_t
		err = unix.Fstat(int(file.Fd()), &st)
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		id := posixIdentityFromStat(&st)
		result = append(result, RuntimeLogFile{
			owner:                 f.owner,
			path:                  filepath.Join(f.layout.RuntimeLogDir(), name),
			name:                  name,
			volumeSerial:          id.directory.volumeSerial,
			fileID:                id.directory.fileID,
			changeTimeSeconds:     id.changeTimeSeconds,
			changeTimeNanoseconds: id.changeTimeNanoseconds,
			generation:            id.generation,
		})
	}
	return result, nil
}

// Remove 仅消费本目录生成且仍绑定相同 inode 的日志令牌。
func (f *RuntimeLogFiles) Remove(ctx context.Context, token RuntimeLogFile) (RemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return RemoveResult{}, ErrClosed
	}
	if token.owner != f.owner || token.name != filepath.Base(token.name) {
		return RemoveResult{}, ErrInvalidToken
	}
	c := f.pins[0].chain
	if err := c.verify(ctx); err != nil {
		return RemoveResult{}, err
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(c.leaf().Fd()), token.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return RemoveResult{}, ErrIdentityChanged
	}
	if err != nil {
		return RemoveResult{}, err
	}
	id := posixIdentityFromStat(&st)
	if id.directory.volumeSerial != token.volumeSerial ||
		id.directory.fileID != token.fileID ||
		id.directory.attributes&unix.S_IFMT != unix.S_IFREG ||
		id.directory.numberOfLinks != 1 ||
		id.changeTimeSeconds != token.changeTimeSeconds ||
		id.changeTimeNanoseconds != token.changeTimeNanoseconds ||
		id.generation != token.generation {
		return RemoveResult{}, ErrIdentityChanged
	}
	err = unix.Unlinkat(int(c.leaf().Fd()), token.name, 0)
	return RemoveResult{MutationApplied: err == nil}, err
}

// Close 幂等关闭日志目录能力。
func (f *RuntimeLogFiles) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closeErr = f.pins[0].chain.close()
	}
	return f.closeErr
}
