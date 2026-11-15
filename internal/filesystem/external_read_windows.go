//go:build windows

package filesystem

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ReadExternalFile 固定完整路径链读取调用方显式选择的普通文件，不跟随 Reparse。
func ReadExternalFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if ctx == nil || limit <= 0 {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api := newProductionPathAPI()
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return nil, err
	}
	root, err := canonicalizeContextWith(ctx, filepath.VolumeName(path)+string(filepath.Separator), api)
	if err != nil {
		return nil, err
	}
	spec := managedFilePinSpec()
	spec.access |= windows.FILE_READ_DATA
	spec.share = windows.FILE_SHARE_READ
	chain, err := openPinnedChainWith(ctx, root, target, spec, api)
	if err != nil {
		return nil, err
	}
	data, readErr := readManagedSourceHandle(ctx, chain.objects[len(chain.objects)-1].handle, limit, api)
	return data, errors.Join(readErr, chain.close())
}
