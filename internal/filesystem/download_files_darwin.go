package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// NewDownloadFiles 验证已有目录链，不创建目录或保留句柄。
func NewDownloadFiles(layout *config.Layout) (*DownloadFiles, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	if _, err := InspectManagedDirectory(context.Background(), layout, layout.DownloadCacheDir()); err != nil {
		return nil, err
	}
	return &DownloadFiles{layout: layout}, nil
}

// Begin 串行准备临时文件，独占 part 的协作锁直到发布或放弃。
func (f *DownloadFiles) Begin(ctx context.Context, name string) (*DownloadSession, error) {
	if f == nil || f.layout == nil || ctx == nil {
		return nil, ErrInvalidArgument
	}
	path, err := f.layout.DownloadFile(name)
	if err != nil {
		return nil, errors.Join(ErrInvalidArgument, err)
	}
	partPath, err := f.layout.DownloadPartFile(name)
	if err != nil {
		return nil, errors.Join(ErrInvalidArgument, err)
	}
	directory, err := openDarwinManagedDirectory(ctx, f.layout, f.layout.DownloadCacheDir())
	if err != nil {
		return nil, err
	}
	var part *os.File
	var identity DirectoryIdentity
	err = withDarwinLock(ctx, directory.file, unix.LOCK_EX, func() error {
		file, _, openErr := directory.openFile(ctx, name, os.O_RDONLY, 0)
		if openErr == nil {
			return errors.Join(ErrDestinationExists, file.Close())
		}
		if !isDarwinStableMissing(openErr) {
			return openErr
		}
		partName := filepath.Base(partPath)
		stale, staleID, staleErr := directory.openFile(ctx, partName, os.O_RDONLY, 0)
		if staleErr == nil {
			lockErr := unix.Flock(int(stale.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if lockErr != nil {
				return errors.Join(lockErr, stale.Close())
			}
			removeErr := directory.removeFile(partName, stale, staleID)
			if err := errors.Join(removeErr, stale.Close()); err != nil {
				return err
			}
		} else if !isDarwinStableMissing(staleErr) {
			return staleErr
		}
		part, identity, openErr = directory.openFile(ctx, partName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return openErr
		}
		return unix.Flock(int(part.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	})
	if err != nil {
		if part != nil {
			err = errors.Join(err, part.Close())
		}
		return nil, joinOperationCleanup(err, directory.file.Close())
	}
	return &DownloadSession{path: path, partPath: partPath, part: pinnedObject{file: part, identity: identity},
		pins: [4]pinnedObject{3: darwinPin(directory)}}, nil
}

// Write 只向当前会话持有且名称仍匹配的普通临时文件写入。
func (s *DownloadSession) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != downloadOpen {
		return 0, ErrClosed
	}
	directory := darwinPinnedDirectory(s.pins[3])
	if _, err := directory.verifyFile(filepath.Base(s.partPath), s.part.file, s.part.identity); err != nil {
		return 0, err
	}
	return s.part.file.Write(data)
}

// Path 返回只供诊断的最终下载文件路径。
func (s *DownloadSession) Path() string { return s.path }

// PartPath 返回只供诊断的临时文件路径。
func (s *DownloadSession) PartPath() string { return s.partPath }

// PublishNoReplace 发布原临时对象，不覆盖竞争目标；发布后错误仍返回 Published=true。
func (s *DownloadSession) PublishNoReplace(ctx context.Context) (PublishResult, error) {
	if ctx == nil {
		return PublishResult{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == downloadPublished {
		return s.published, s.closeErr
	}
	if s.state != downloadOpen {
		return PublishResult{}, ErrClosed
	}
	directory := darwinPinnedDirectory(s.pins[3])
	if _, err := directory.verifyFile(filepath.Base(s.partPath), s.part.file, s.part.identity); err != nil {
		return PublishResult{}, err
	}
	if err := s.part.file.Sync(); err != nil {
		return PublishResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	fd := int(directory.file.Fd())
	if err := unix.RenameatxNp(fd, filepath.Base(s.partPath), fd, filepath.Base(s.path), unix.RENAME_EXCL); err != nil {
		if errors.Is(err, os.ErrExist) {
			return PublishResult{}, errors.Join(ErrDestinationExists, err)
		}
		return PublishResult{}, err
	}
	s.state = downloadPublished
	s.published = PublishResult{Published: true}
	identityErr := directory.verifyName(filepath.Base(s.path), s.part.identity, false)
	s.closeErr = errors.Join(identityErr, directory.file.Sync(), closeDarwinPins([]pinnedObject{s.part}), closeDarwinPins(s.pins[:]))
	s.part.file = nil
	return s.published, s.closeErr
}

// Abort 只移除会话自己的 part，对名称替换保留现场并收口 FD。
func (s *DownloadSession) Abort(ctx context.Context) (AbortResult, error) {
	if ctx == nil {
		return AbortResult{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return AbortResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == downloadPublished {
		return AbortResult{}, s.closeErr
	}
	if s.state == downloadAborted {
		return s.abort, s.closeErr
	}
	directory := darwinPinnedDirectory(s.pins[3])
	err := directory.removeFile(filepath.Base(s.partPath), s.part.file, s.part.identity)
	if err == nil {
		s.abort.Removed = true
		err = directory.file.Sync()
	}
	s.state = downloadAborted
	s.closeErr = errors.Join(err, closeDarwinPins([]pinnedObject{s.part}), closeDarwinPins(s.pins[:]))
	s.part.file = nil
	return s.abort, s.closeErr
}
