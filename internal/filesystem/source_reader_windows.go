//go:build windows

package filesystem

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ManagedSourceReader 在一次顺序扫描内复用公共祖先目录句柄，不得跨协程共享。
// 所有目录持续拒绝共享删除；叶子文件只在一次 Read 期间打开。
type ManagedSourceReader struct {
	root      CanonicalPath
	chain     *pinnedChain
	rootCount int
	closed    bool
}

// NewManagedSourceReader 固定 repo 及全部受管祖先；调用方必须关闭读取器。
func NewManagedSourceReader(ctx context.Context, layout *config.Layout) (*ManagedSourceReader, error) {
	return newManagedSourceReaderWithAPI(ctx, layout, newProductionPathAPI())
}

func newManagedSourceReaderWithAPI(ctx context.Context, layout *config.Layout, api pathAPI) (*ManagedSourceReader, error) {
	if ctx == nil || layout == nil || !api.valid() {
		return nil, ErrInvalidArgument
	}
	root, err := canonicalizeContextWith(ctx, layout.RepoDir(), api)
	if err != nil {
		return nil, err
	}
	parent, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return nil, err
	}
	chain, err := openPinnedChainWith(ctx, parent, root, directoryPinSpec(), api)
	if err != nil {
		return nil, err
	}
	return &ManagedSourceReader{root: root, chain: chain, rootCount: len(chain.objects)}, nil
}

// Read 沿已固定目录相对打开普通文件，复用前重新核对身份和可变安全属性。
func (r *ManagedSourceReader) Read(ctx context.Context, path string, limit int64) ([]byte, error) {
	if ctx == nil || r == nil || r.chain == nil || limit < 0 {
		return nil, ErrInvalidArgument
	}
	if r.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := canonicalizeContextWith(ctx, path, r.chain.api)
	if err != nil {
		return nil, err
	}
	if !r.root.Contains(target) {
		return nil, outsidePathError("read-source", target.String(), ErrIdentityChanged)
	}
	keep := len(r.chain.objects)
	for keep > r.rootCount && !r.chain.objects[keep-1].path.Contains(target) {
		keep--
	}
	if err := r.closeSuffix(keep); err != nil {
		return nil, err
	}
	// 目录句柄阻止改名和替换，但不把 Reparse 或大小写属性当成不可变值缓存。
	for _, object := range r.chain.objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identity, err := r.chain.api.identity(object.handle)
		if err != nil {
			return nil, wrapFileError("identify-cached-directory", object.path.String(), err)
		}
		if !matchesDirectoryIdentity(&object.identity, identity) {
			return nil, wrapFileError("validate-cached-directory", object.path.String(), ErrIdentityChanged)
		}
		object.identity = identity
		if err := validatePinnedDirectory(ctx, object, r.chain.api); err != nil {
			return nil, err
		}
	}
	components := canonicalComponents(target.display)
	parentDepth := len(canonicalComponents(r.chain.objects[len(r.chain.objects)-1].path.display))
	for i := parentDepth; i < len(components)-1; i++ {
		parent := r.chain.objects[len(r.chain.objects)-1]
		child, err := openRelativeCheckedWith(ctx, parent, components[i], directoryPinSpec(), r.chain.api)
		if err != nil {
			return nil, err
		}
		r.chain.objects = append(r.chain.objects, child)
	}
	spec := managedFilePinSpec()
	spec.access |= windows.FILE_READ_DATA
	spec.share = windows.FILE_SHARE_READ
	parent := r.chain.objects[len(r.chain.objects)-1]
	leaf, err := openRelativeCheckedWith(ctx, parent, components[len(components)-1], spec, r.chain.api)
	if err != nil {
		return nil, err
	}
	r.chain.objects = append(r.chain.objects, leaf)
	if !leaf.path.Equal(target) {
		return nil, errors.Join(outsidePathError("pin", target.String(), ErrIdentityChanged), r.closeSuffix(len(r.chain.objects)-1))
	}
	data, readErr := readManagedSourceHandle(ctx, leaf.handle, limit, r.chain.api)
	if err := errors.Join(readErr, r.closeSuffix(len(r.chain.objects)-1)); err != nil {
		return nil, err
	}
	return data, nil
}

func (r *ManagedSourceReader) closeSuffix(keep int) error {
	suffix := pinnedChain{api: r.chain.api, objects: r.chain.objects[keep:]}
	if err := suffix.close(); err != nil {
		// 部分关闭后只允许 Close 重试，不能继续使用链中失效的句柄。
		r.closed = true
		return err
	}
	r.chain.objects = r.chain.objects[:keep]
	return nil
}

// Close 释放目录链；失败时保留未关闭句柄，后续 Close 可以重试。
func (r *ManagedSourceReader) Close() error {
	if r == nil || r.chain == nil {
		return nil
	}
	r.closed = true
	return r.chain.close()
}
