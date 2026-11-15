//go:build linux || darwin

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// NewDownloadFiles 创建受管下载能力。
func NewDownloadFiles(layout *config.Layout) (*DownloadFiles, error) {
	if layout == nil {
		return nil, ErrInvalidArgument
	}
	return &DownloadFiles{layout: layout}, nil
}

// Begin 独占创建临时文件，拒绝覆盖已有下载。
func (f *DownloadFiles) Begin(ctx context.Context, name string) (*DownloadSession, error) {
	if f == nil || ctx == nil {
		return nil, ErrInvalidArgument
	}
	path, err := f.layout.DownloadFile(name)
	if err != nil {
		return nil, err
	}
	part, err := f.layout.DownloadPartFile(name)
	if err != nil {
		return nil, err
	}
	c, err := managedPOSIX(ctx, f.layout, f.layout.DownloadCacheDir(), true)
	if err != nil {
		return nil, err
	}
	_, exists, err := posixEntryIdentity(c.leaf(), filepath.Base(path))
	if exists {
		err = ErrDestinationExists
	}
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	file, err := openPOSIXFile(c, filepath.Base(part), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	id, err := posixIdentity(file)
	if err != nil {
		return nil, errors.Join(err, file.Close(), c.close())
	}
	s := &DownloadSession{path: path, partPath: part, part: pinnedObject{file: file, identity: id}, state: downloadOpen}
	s.pins[0].chain = c
	return s, nil
}

// Write 写入当前固定的下载对象。
func (s *DownloadSession) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != downloadOpen {
		return 0, ErrClosed
	}
	return s.part.file.Write(data)
}

// Path 返回发布目标路径。
func (s *DownloadSession) Path() string { return s.path }

// PartPath 返回临时文件路径。
func (s *DownloadSession) PartPath() string { return s.partPath }

// PublishNoReplace 同步文件并原子发布，保留目标的独占语义。
func (s *DownloadSession) PublishNoReplace(ctx context.Context) (PublishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == downloadPublished {
		return s.published, s.closeErr
	}
	if s.state != downloadOpen {
		return PublishResult{}, ErrClosed
	}
	c := s.pins[0].chain
	err := c.verify(ctx)
	if err == nil {
		err = s.part.file.Sync()
	}
	if err == nil {
		id, exists, e := posixEntryIdentity(c.leaf(), filepath.Base(s.partPath))
		err = e
		if !exists || !s.part.identity.Equal(&id) {
			err = ErrIdentityChanged
		}
	}
	if err == nil {
		err = renamePOSIXNoReplace(int(c.leaf().Fd()), filepath.Base(s.partPath), int(c.leaf().Fd()), filepath.Base(s.path))
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			err = errors.Join(ErrDestinationExists, err)
		}
		return PublishResult{}, err
	}
	s.state = downloadPublished
	s.published = PublishResult{Published: true}
	s.closeErr = errors.Join(c.leaf().Sync(), s.part.file.Close(), c.close())
	return s.published, s.closeErr
}

// Abort 仅移除仍与下载句柄绑定的临时文件。
func (s *DownloadSession) Abort(ctx context.Context) (AbortResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == downloadAborted {
		return s.abort, s.closeErr
	}
	if s.state == downloadPublished {
		return AbortResult{}, nil
	}
	c := s.pins[0].chain
	err := c.verify(ctx)
	if err == nil {
		id, exists, e := posixEntryIdentity(c.leaf(), filepath.Base(s.partPath))
		err = e
		if !exists || !s.part.identity.Equal(&id) {
			err = ErrIdentityChanged
		}
	}
	if err == nil {
		err = unix.Unlinkat(int(c.leaf().Fd()), filepath.Base(s.partPath), 0)
	}
	s.abort = AbortResult{Removed: err == nil}
	s.state = downloadAborted
	s.closeErr = errors.Join(err, s.part.file.Close(), c.close())
	return s.abort, s.closeErr
}
