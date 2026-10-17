package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// NewRuntimeLogFiles 创建专用日志目录，不授权其他 debug 子目录。
func NewRuntimeLogFiles(ctx context.Context, layout *config.Layout) (*RuntimeLogFiles, error) {
	if ctx == nil || layout == nil {
		return nil, ErrInvalidArgument
	}
	directory, err := openDarwinManagedDirectory(ctx, layout, layout.RuntimeLogDir())
	if err != nil {
		return nil, err
	}
	return &RuntimeLogFiles{layout: layout, owner: &runtimeLogOwner{marker: 1},
		pins: [3]pinnedObject{2: darwinPin(directory)}}, nil
}

// OpenAppend 对日志持有共享协作锁，writer 的目录 FD 与父能力独立。
func (f *RuntimeLogFiles) OpenAppend(ctx context.Context, command string, date time.Time) (*RuntimeLogWriter, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	path, err := f.layout.RuntimeLogFile(command, date)
	if err != nil {
		return nil, errors.Join(ErrInvalidArgument, err)
	}
	directory, err := darwinPinnedDirectory(f.pins[2]).openDirectory(ctx, ".", false)
	if err != nil {
		return nil, err
	}
	var file *os.File
	var identity DirectoryIdentity
	err = withDarwinLock(ctx, directory.file, unix.LOCK_EX, func() error {
		var openErr error
		file, identity, openErr = directory.openFile(ctx, filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if openErr != nil {
			return openErr
		}
		return unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	})
	if err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, joinOperationCleanup(err, directory.file.Close())
	}
	return &RuntimeLogWriter{path: path, file: pinnedObject{file: file, identity: identity},
		pins: [3]pinnedObject{2: darwinPin(directory)}}, nil
}

// List 只颁发同卷、单链接普通文件的身份令牌。
func (f *RuntimeLogFiles) List(ctx context.Context) ([]RuntimeLogFile, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	directory := darwinPinnedDirectory(f.pins[2])
	var result []RuntimeLogFile
	err := withDarwinLock(ctx, directory.file, unix.LOCK_SH, func() error {
		reader, err := directory.openDirectory(ctx, ".", false)
		if err != nil {
			return err
		}
		entries, readErr := reader.file.ReadDir(-1)
		if err := errors.Join(readErr, reader.file.Close()); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !entry.Type().IsRegular() {
				continue
			}
			file, identity, err := directory.openFile(ctx, entry.Name(), os.O_RDONLY, 0)
			if errors.Is(err, ErrUnsafeHardLink) || isDarwinStableMissing(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			result = append(result, RuntimeLogFile{owner: f.owner, name: entry.Name(),
				path: filepath.Join(f.layout.RuntimeLogDir(), entry.Name()), volumeSerial: identity.volumeSerial, fileID: identity.fileID})
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, err
}

// Remove 拒绝其他能力的令牌、已替换对象和仍有 writer 的日志。
func (f *RuntimeLogFiles) Remove(ctx context.Context, token RuntimeLogFile) (RemoveResult, error) {
	if ctx == nil {
		return RemoveResult{}, ErrInvalidArgument
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return RemoveResult{}, ErrClosed
	}
	if token.owner != f.owner || token.owner == nil || token.name == "" || filepath.Base(token.name) != token.name ||
		token.path != filepath.Join(f.layout.RuntimeLogDir(), token.name) {
		return RemoveResult{}, ErrInvalidToken
	}
	directory := darwinPinnedDirectory(f.pins[2])
	var result RemoveResult
	err := withDarwinLock(ctx, directory.file, unix.LOCK_EX, func() error {
		file, identity, err := directory.openFile(ctx, token.name, os.O_RDONLY, 0)
		if isDarwinStableMissing(err) {
			return nil
		}
		if err != nil {
			return err
		}
		expected := DirectoryIdentity{volumeSerial: token.volumeSerial, fileID: token.fileID}
		if !identity.Equal(&expected) {
			return errors.Join(ErrIdentityChanged, file.Close())
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return errors.Join(err, file.Close())
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(err, file.Close())
		}
		removeErr := directory.removeFile(token.name, file, expected)
		if removeErr == nil {
			result.MutationApplied = true
			removeErr = directory.file.Sync()
		}
		return errors.Join(removeErr, file.Close())
	})
	return result, err
}

// Close 关闭目录能力，已打开的 writer 仍持有自己的 FD。
func (f *RuntimeLogFiles) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closeErr = closeDarwinPins(f.pins[:])
	}
	return f.closeErr
}

// Write 在目录协作锁内原子检查容量并追加完整记录。
func (w *RuntimeLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	directory := darwinPinnedDirectory(w.pins[2])
	var written int
	err := withDarwinLock(ctx, directory.file, unix.LOCK_EX, func() error {
		identity, err := directory.verifyFile(filepath.Base(w.path), w.file.file, w.file.identity)
		if err != nil {
			return err
		}
		if identity.size < 0 || int64(len(data)) > MaxRuntimeLogBytes-identity.size {
			return ErrRuntimeLogCapacity
		}
		written, err = w.file.file.Write(data)
		return err
	})
	return written, err
}

// Path 返回只供诊断的日志路径。
func (w *RuntimeLogWriter) Path() string { return w.path }

// Close 幂等释放 writer 自己的文件与目录能力。
func (w *RuntimeLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		w.closeErr = errors.Join(closeDarwinPins([]pinnedObject{w.file}), closeDarwinPins(w.pins[:]))
		w.file.file = nil
	}
	return w.closeErr
}
