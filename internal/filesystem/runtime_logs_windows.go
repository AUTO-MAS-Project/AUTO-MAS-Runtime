package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// NewRuntimeLogFiles 创建固定并验证 Runtime 日志目录的能力。
func NewRuntimeLogFiles(ctx context.Context, layout *config.Layout) (*RuntimeLogFiles, error) {
	return newRuntimeLogFilesWith(ctx, layout, runtimeLogDependencies{api: newProductionPathAPI()})
}

func newRuntimeLogFilesWith(
	ctx context.Context,
	layout *config.Layout,
	dependencies runtimeLogDependencies,
) (*RuntimeLogFiles, error) {
	if ctx == nil || layout == nil || !dependencies.api.valid() {
		return nil, fmt.Errorf("%w: invalid runtime-log dependencies", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api := dependencies.api
	appPath, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return nil, err
	}
	parentPath, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return nil, err
	}
	appChain, err := openPinnedChainWith(ctx, parentPath, appPath, directoryPinSpec(), api)
	if err != nil {
		return nil, err
	}
	app, err := detachLeaf(appChain)
	if err != nil {
		return nil, err
	}
	pins := make([]pinnedObject, 0, 3)
	pins = append(pins, app)
	fail := func(operationErr error) (*RuntimeLogFiles, error) {
		return nil, errors.Join(operationErr, closePinnedObjects(api, pins))
	}

	parent := app
	for _, path := range []string{layout.LogsDir(), layout.RuntimeLogDir()} {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		canonical, err := canonicalizeContextWith(ctx, path, api)
		if err != nil {
			return fail(err)
		}
		child, err := openOrCreatePinnedDirectory(ctx, parent, canonical, api)
		if err != nil {
			return fail(err)
		}
		pins = append(pins, child)
		parent = child
	}
	return &RuntimeLogFiles{
		layout: layout,
		api:    api,
		owner:  &runtimeLogOwner{marker: 1},
		pins:   [3]pinnedObject{pins[0], pins[1], pins[2]},
	}, nil
}

func openOrCreatePinnedDirectory(
	ctx context.Context,
	parent pinnedObject,
	expected CanonicalPath,
	api pathAPI,
) (pinnedObject, error) {
	if err := ctx.Err(); err != nil {
		return pinnedObject{}, err
	}
	name := filepath.Base(expected.String())
	child, err := openRelativeCheckedWith(ctx, parent, name, directoryPinSpec(), api)
	if err == nil {
		if !child.path.Equal(expected) {
			closeErr := api.closeHandle(child.handle)
			return pinnedObject{}, errors.Join(
				outsidePathError("open-directory", expected.String(), ErrIdentityChanged),
				wrapFileError("close", child.path.String(), closeErr),
			)
		}
		return child, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return pinnedObject{}, err
	}
	if err := ctx.Err(); err != nil {
		return pinnedObject{}, err
	}
	if err := api.makeDirectory(expected.Native()); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return pinnedObject{}, &FileError{Operation: "create-directory", Path: expected.String(), Err: err}
	}
	child, err = openRelativeCheckedWith(context.WithoutCancel(ctx), parent, name, directoryPinSpec(), api)
	if err != nil {
		return pinnedObject{}, err
	}
	if !child.path.Equal(expected) {
		closeErr := api.closeHandle(child.handle)
		return pinnedObject{}, errors.Join(
			outsidePathError("create-directory", expected.String(), ErrIdentityChanged),
			wrapFileError("close", child.path.String(), closeErr),
		)
	}
	return child, nil
}

func detachLeaf(chain *pinnedChain) (pinnedObject, error) {
	if chain == nil || len(chain.objects) == 0 {
		return pinnedObject{}, fmt.Errorf("%w: empty pinned chain", ErrInvalidArgument)
	}
	last := len(chain.objects) - 1
	leaf := chain.objects[last]
	chain.objects[last].handle = windows.InvalidHandle
	if err := chain.close(); err != nil {
		leafCloseErr := chain.api.closeHandle(leaf.handle)
		return pinnedObject{}, errors.Join(err, wrapFileError("close", leaf.path.String(), leafCloseErr))
	}
	return leaf, nil
}

func closePinnedObjects(api pathAPI, objects []pinnedObject) error {
	closeErrors := make([]error, 0, len(objects))
	for i := len(objects) - 1; i >= 0; i-- {
		handle := &objects[i].handle
		if *handle == 0 || *handle == windows.InvalidHandle {
			continue
		}
		if err := api.closeHandle(*handle); err != nil {
			closeErrors = append(closeErrors, &FileError{Operation: "close", Path: objects[i].path.String(), Err: err})
			continue
		}
		*handle = windows.InvalidHandle
	}
	return errors.Join(closeErrors...)
}

func closePinnedObject(api pathAPI, object *pinnedObject) error {
	if object.handle == 0 || object.handle == windows.InvalidHandle {
		return nil
	}
	if err := api.closeHandle(object.handle); err != nil {
		return &FileError{Operation: "close", Path: object.path.String(), Err: err}
	}
	object.handle = windows.InvalidHandle
	return nil
}

// OpenAppend 打开以本地日期命名的 Runtime 日志，并保留所有目录祖先的独立句柄。
func (f *RuntimeLogFiles) OpenAppend(
	ctx context.Context,
	command string,
	localDate time.Time,
) (*RuntimeLogWriter, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	path, err := f.layout.RuntimeLogFile(command, localDate)
	if err != nil {
		return nil, fmt.Errorf("%w: runtime log path: %v", ErrInvalidArgument, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	spec := openSpec{
		access:    windows.FILE_APPEND_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_ALWAYS,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	leaf, err := openRelativeCheckedWith(context.WithoutCancel(ctx), f.pins[2], filepath.Base(path), spec, f.api)
	if err != nil {
		return nil, err
	}
	if leaf.identity.numberOfLinks != 1 {
		closeErr := f.api.closeHandle(leaf.handle)
		return nil, errors.Join(
			&FileError{Operation: "link-count", Path: path, Err: ErrUnsafeHardLink},
			wrapFileError("close", path, closeErr),
		)
	}

	writerPins := make([]pinnedObject, 0, len(f.pins))
	for i := range f.pins {
		duplicate, err := f.api.duplicateHandle(f.pins[i].handle)
		if err != nil {
			return nil, errors.Join(
				&FileError{Operation: "duplicate", Path: f.pins[i].path.String(), Err: err},
				closePinnedObjects(f.api, writerPins),
				wrapFileError("close", path, f.api.closeHandle(leaf.handle)),
			)
		}
		writerPins = append(writerPins, pinnedObject{path: f.pins[i].path, handle: duplicate, identity: f.pins[i].identity})
	}
	return &RuntimeLogWriter{
		api:  f.api,
		path: path,
		file: leaf,
		pins: [3]pinnedObject{writerPins[0], writerPins[1], writerPins[2]},
	}, nil
}

// List 返回 Runtime 日志目录中已经按对象身份验证的直接日志文件令牌。
func (f *RuntimeLogFiles) List(ctx context.Context) ([]RuntimeLogFile, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrClosed
	}
	entries, err := f.api.listDirectory(f.pins[2].handle)
	if err != nil {
		return nil, &FileError{
			Operation: "list",
			Path:      f.layout.RuntimeLogDir(),
			Err:       err,
		}
	}
	result := make([]RuntimeLogFile, 0, len(entries))
	spec := openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if filepath.Base(entry.name) != entry.name {
			return nil, &FileError{
				Operation: "list",
				Path:      f.layout.RuntimeLogDir(),
				Err:       ErrIdentityChanged,
			}
		}
		if entry.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
			continue
		}
		leaf, err := openRelativeCheckedWith(ctx, f.pins[2], entry.name, spec, f.api)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_DIRECTORY) {
			continue
		}
		if err != nil {
			var stable *Error
			if errors.As(err, &stable) && stable.Code() == protocol.CodeUnsafeReparsePoint {
				continue
			}
			return nil, err
		}
		path := filepath.Join(f.layout.RuntimeLogDir(), entry.name)
		if leaf.identity.numberOfLinks == 1 {
			result = append(result, RuntimeLogFile{
				owner:        f.owner,
				path:         path,
				name:         entry.name,
				volumeSerial: leaf.identity.volumeSerial,
				fileID:       leaf.identity.fileID,
			})
		}
		if err := f.api.closeHandle(leaf.handle); err != nil {
			return nil, &FileError{Operation: "close", Path: path, Err: err}
		}
	}
	return result, nil
}

// Remove 按令牌中的对象身份条件删除已列出的 Runtime 日志文件。
func (f *RuntimeLogFiles) Remove(
	ctx context.Context,
	file RuntimeLogFile,
) (RemoveResult, error) {
	if ctx == nil {
		return RemoveResult{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return RemoveResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return RemoveResult{}, ErrClosed
	}
	if file.owner == nil || file.owner != f.owner || file.name == "" || file.path == "" {
		return RemoveResult{}, ErrInvalidToken
	}
	expectedPath := filepath.Join(f.layout.RuntimeLogDir(), file.name)
	expected, err := canonicalizeContextWith(ctx, expectedPath, f.api)
	if err != nil {
		return RemoveResult{}, err
	}
	tokenPath, err := canonicalizeContextWith(ctx, file.path, f.api)
	if err != nil {
		return RemoveResult{}, ErrInvalidToken
	}
	if !expected.Equal(tokenPath) {
		return RemoveResult{}, ErrInvalidToken
	}
	if err := ctx.Err(); err != nil {
		return RemoveResult{}, err
	}
	spec := openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	leaf, err := openRelativeCheckedWith(ctx, f.pins[2], file.name, spec, f.api)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return RemoveResult{}, nil
	}
	if err != nil {
		return RemoveResult{}, err
	}
	if leaf.identity.volumeSerial != file.volumeSerial ||
		leaf.identity.fileID != file.fileID ||
		leaf.identity.numberOfLinks != 1 {
		closeErr := f.api.closeHandle(leaf.handle)
		return RemoveResult{}, errors.Join(
			ErrIdentityChanged,
			wrapFileError("close", file.path, closeErr),
		)
	}
	if err := ctx.Err(); err != nil {
		closeErr := f.api.closeHandle(leaf.handle)
		return RemoveResult{}, errors.Join(err, wrapFileError("close", file.path, closeErr))
	}
	if err := deleteByHandleWith(leaf.handle, f.api); err != nil {
		closeErr := f.api.closeHandle(leaf.handle)
		return RemoveResult{}, errors.Join(
			&FileError{Operation: "remove", Path: file.path, Err: err},
			wrapFileError("close", file.path, closeErr),
		)
	}
	result := RemoveResult{MutationApplied: true}
	if err := f.api.closeHandle(leaf.handle); err != nil {
		return result, &FileError{Operation: "close", Path: file.path, Err: err}
	}
	return result, nil
}

// Write 将内容附加到已验证的单链接日志文件。
func (w *RuntimeLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	identity, err := w.api.identity(w.file.handle)
	if err != nil {
		return 0, &FileError{Operation: "identify", Path: w.path, Err: err}
	}
	if identity.volumeSerial != w.file.identity.volumeSerial || identity.fileID != w.file.identity.fileID {
		return 0, &FileError{Operation: "identify", Path: w.path, Err: ErrIdentityChanged}
	}
	if identity.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || identity.numberOfLinks != 1 {
		return 0, &FileError{Operation: "link-count", Path: w.path, Err: ErrUnsafeHardLink}
	}
	n, err := w.api.writeFile(w.file.handle, p)
	if err != nil {
		return n, &FileError{Operation: "write", Path: w.path, Err: err}
	}
	return n, nil
}

// Path 返回日志文件的绝对路径。
func (w *RuntimeLogWriter) Path() string {
	return w.path
}

// Close 关闭日志文件及该 writer 独占的祖先 pin。
func (w *RuntimeLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.closeErr = errors.Join(closePinnedObject(w.api, &w.file), closePinnedObjects(w.api, w.pins[:]))
	return w.closeErr
}

// Close 关闭父能力自身的目录 pin，不影响已打开 writer 的独立 pin。
func (f *RuntimeLogFiles) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closeErr
	}
	f.closed = true
	f.closeErr = closePinnedObjects(f.api, f.pins[:])
	return f.closeErr
}
