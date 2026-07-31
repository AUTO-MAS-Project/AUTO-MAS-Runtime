package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// NewDownloadFiles 创建下载目录能力，但不创建目录或保留句柄。
func NewDownloadFiles(layout *config.Layout) (*DownloadFiles, error) {
	return newDownloadFilesWith(layout, downloadFileDependencies{
		api: newProductionPathAPI(),
	})
}

func newDownloadFilesWith(
	layout *config.Layout,
	dependencies downloadFileDependencies,
) (*DownloadFiles, error) {
	if layout == nil || !dependencies.api.valid() {
		return nil, fmt.Errorf("%w: invalid download-file dependencies", ErrInvalidArgument)
	}
	ctx := context.Background()
	targets := []string{
		layout.AppRoot(),
		layout.RuntimeDir(),
		layout.RuntimeCacheDir(),
		layout.DownloadCacheDir(),
	}
	for _, target := range targets {
		if err := validateExistingLayoutPathWith(ctx, layout, target, dependencies.api); err != nil {
			return nil, err
		}
	}
	return &DownloadFiles{layout: layout, api: dependencies.api}, nil
}

func validateExistingLayoutPathWith(
	ctx context.Context,
	layout *config.Layout,
	target string,
	api pathAPI,
) error {
	appRoot, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return err
	}
	canonical, err := canonicalizeContextWith(ctx, target, api)
	if err != nil {
		return err
	}
	if !appRoot.Equal(canonical) && !appRoot.Contains(canonical) {
		return outsidePathError("validate-download-root", canonical.String(), ErrIdentityChanged)
	}
	existing := canonical.String()
	for {
		_, err := api.attributes(nativeWindowsPath(existing))
		if err == nil {
			break
		}
		if !isWindowsNotFound(err) {
			return &FileError{Operation: "attributes", Path: existing, Err: err}
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return &FileError{Operation: "attributes", Path: existing, Err: err}
		}
		existing = parent
	}
	existingCanonical, err := canonicalizeContextWith(ctx, existing, api)
	if err != nil {
		return err
	}
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return err
	}
	chain, err := openPinnedChainWith(
		ctx,
		rootParent,
		existingCanonical,
		directoryPinSpec(),
		api,
	)
	if err != nil {
		return err
	}
	return chain.close()
}

// Begin 创建并固定一个新的下载临时文件会话。
func (f *DownloadFiles) Begin(
	ctx context.Context,
	name string,
) (*DownloadSession, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalPath, err := f.layout.DownloadFile(name)
	if err != nil {
		return nil, fmt.Errorf("%w: download final path: %v", ErrInvalidArgument, err)
	}
	partPath, err := f.layout.DownloadPartFile(name)
	if err != nil {
		return nil, fmt.Errorf("%w: download part path: %v", ErrInvalidArgument, err)
	}
	pins, err := f.pinDownloadRoots(ctx)
	if err != nil {
		return nil, err
	}
	fail := func(operationErr error) (*DownloadSession, error) {
		return nil, errors.Join(operationErr, closePinnedObjects(f.api, pins[:]))
	}

	finalSpec := openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	final, err := openRelativeCheckedWith(
		ctx,
		pins[3],
		filepath.Base(finalPath),
		finalSpec,
		f.api,
	)
	if err == nil {
		closeErr := closePinnedObject(f.api, &final)
		if final.identity.numberOfLinks != 1 {
			return fail(errors.Join(ErrUnsafeHardLink, closeErr))
		}
		return fail(errors.Join(ErrDestinationExists, closeErr))
	}
	if !isWindowsNotFound(err) {
		return fail(err)
	}

	staleSpec := openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	stale, err := openRelativeCheckedWith(
		ctx,
		pins[3],
		filepath.Base(partPath),
		staleSpec,
		f.api,
	)
	if err == nil {
		if stale.identity.numberOfLinks != 1 {
			return fail(errors.Join(
				ErrUnsafeHardLink,
				closePinnedObject(f.api, &stale),
			))
		}
		if err := ctx.Err(); err != nil {
			return fail(errors.Join(err, closePinnedObject(f.api, &stale)))
		}
		if err := deleteByHandleWith(stale.handle, f.api); err != nil {
			return fail(errors.Join(err, closePinnedObject(f.api, &stale)))
		}
		if err := closePinnedObject(f.api, &stale); err != nil {
			return fail(err)
		}
	} else if !isWindowsNotFound(err) {
		return fail(err)
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	partSpec := openSpec{
		access: windows.FILE_WRITE_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ,
		creation:  windows.CREATE_NEW,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	part, err := openRelativeCheckedWith(
		context.WithoutCancel(ctx),
		pins[3],
		filepath.Base(partPath),
		partSpec,
		f.api,
	)
	if err != nil {
		return fail(err)
	}
	if part.identity.numberOfLinks != 1 {
		cleanupErr := errors.Join(
			deleteByHandleWith(part.handle, f.api),
			closePinnedObject(f.api, &part),
		)
		return fail(errors.Join(ErrUnsafeHardLink, cleanupErr))
	}
	return &DownloadSession{
		api:      f.api,
		path:     finalPath,
		partPath: partPath,
		part:     part,
		pins:     pins,
		state:    downloadOpen,
	}, nil
}

func (f *DownloadFiles) pinDownloadRoots(
	ctx context.Context,
) ([4]pinnedObject, error) {
	var result [4]pinnedObject
	appPath, err := canonicalizeContextWith(ctx, f.layout.AppRoot(), f.api)
	if err != nil {
		return result, err
	}
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(f.layout.AppRoot()), f.api)
	if err != nil {
		return result, err
	}
	appChain, err := openPinnedChainWith(
		ctx,
		rootParent,
		appPath,
		directoryPinSpec(),
		f.api,
	)
	if err != nil {
		return result, err
	}
	app, err := detachLeaf(appChain)
	if err != nil {
		return result, err
	}
	pins := make([]pinnedObject, 0, 4)
	pins = append(pins, app)
	parent := app
	targets := []string{
		f.layout.RuntimeDir(),
		f.layout.RuntimeCacheDir(),
		f.layout.DownloadCacheDir(),
	}
	for _, target := range targets {
		canonical, err := canonicalizeContextWith(ctx, target, f.api)
		if err != nil {
			return result, errors.Join(err, closePinnedObjects(f.api, pins))
		}
		child, err := openOrCreatePinnedDirectory(ctx, parent, canonical, f.api)
		if err != nil {
			return result, errors.Join(err, closePinnedObjects(f.api, pins))
		}
		pins = append(pins, child)
		parent = child
	}
	copy(result[:], pins)
	return result, nil
}

// Write 向已固定的临时文件写入字节。
func (s *DownloadSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != downloadOpen {
		return 0, ErrClosed
	}
	identity, err := s.api.identity(s.part.handle)
	if err != nil {
		return 0, &FileError{Operation: "identify", Path: s.partPath, Err: err}
	}
	if identity.volumeSerial != s.part.identity.volumeSerial ||
		identity.fileID != s.part.identity.fileID {
		return 0, ErrIdentityChanged
	}
	if identity.numberOfLinks != 1 ||
		identity.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return 0, ErrUnsafeHardLink
	}
	n, err := s.api.writeFile(s.part.handle, p)
	if err != nil {
		return n, &FileError{Operation: "write", Path: s.partPath, Err: err}
	}
	return n, nil
}

// Path 返回最终下载文件路径。
func (s *DownloadSession) Path() string {
	return s.path
}

// PartPath 返回临时下载文件路径。
func (s *DownloadSession) PartPath() string {
	return s.partPath
}

// Abort 删除仍处于 open 状态的临时文件并关闭会话。
func (s *DownloadSession) Abort(ctx context.Context) (AbortResult, error) {
	if ctx == nil {
		return AbortResult{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return AbortResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case downloadAborted:
		return s.abort, s.closeErr
	case downloadPublished:
		return AbortResult{}, ErrClosed
	case downloadOpen:
	default:
		return AbortResult{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return AbortResult{}, err
	}
	if err := deleteByHandleWith(s.part.handle, s.api); err != nil {
		return AbortResult{}, &FileError{Operation: "abort", Path: s.partPath, Err: err}
	}
	s.state = downloadAborted
	s.abort = AbortResult{Removed: true}
	s.closeErr = errors.Join(
		closePinnedObject(s.api, &s.part),
		closePinnedObjects(s.api, s.pins[:]),
	)
	return s.abort, s.closeErr
}
