//go:build windows

package lock

import (
	"context"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	rootAccess          = uint32(windows.FILE_READ_ATTRIBUTES)
	rootShare           = uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	rootCreation        = uint32(windows.OPEN_EXISTING)
	rootFlags           = uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	mutexWaitTimeout    = uint32(0)
	threadWait          = uint32(windows.INFINITE)
	threadAccess        = uint32(windows.SYNCHRONIZE)
	waitResultObject0   = uint32(windows.WAIT_OBJECT_0)
	waitResultAbandoned = uint32(windows.WAIT_ABANDONED)
	waitResultTimeout   = uint32(windows.WAIT_TIMEOUT)
	waitResultFailed    = uint32(windows.WAIT_FAILED)
)

type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

type rootResource struct {
	handle   windows.Handle
	identity rootIdentity
}

func openRootIdentity(
	ctx context.Context,
	api windowsAPI,
	path string,
) (rootResource, error) {
	if ctx == nil {
		return rootResource{}, errors.New("open app root: nil context")
	}
	if err := ctx.Err(); err != nil {
		return rootResource{}, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return rootResource{}, &OperationError{
			Operation: "encode-root",
			Cause:     err,
		}
	}
	handle, err := api.createFile(
		name,
		rootAccess,
		rootShare,
		nil,
		rootCreation,
		rootFlags,
		0,
	)
	if err != nil {
		return rootResource{}, &OperationError{
			Operation: "open-root",
			Cause:     err,
		}
	}
	closeOnFailure := func(cause error) (rootResource, error) {
		closeErr := api.closeHandle(handle)
		if closeErr != nil {
			closeErr = &OperationError{
				Operation: "close-root",
				Cause:     closeErr,
			}
		}
		return rootResource{}, errors.Join(cause, closeErr)
	}

	if err := ctx.Err(); err != nil {
		return closeOnFailure(err)
	}
	var basic windows.ByHandleFileInformation
	if err := api.getFileInformationByHandle(handle, &basic); err != nil {
		return closeOnFailure(&OperationError{
			Operation: "inspect-root",
			Cause:     err,
		})
	}
	if basic.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return closeOnFailure(&OperationError{
			Operation: "inspect-root",
			Cause:     errors.New("app root is not a directory"),
		})
	}
	if err := ctx.Err(); err != nil {
		return closeOnFailure(err)
	}
	var fileID fileIDInfo
	if err := api.getFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&fileID)),
		uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return closeOnFailure(&OperationError{
			Operation: "read-root-file-id",
			Cause:     err,
		})
	}
	if err := ctx.Err(); err != nil {
		return closeOnFailure(err)
	}
	return rootResource{
		handle: handle,
		identity: rootIdentity{
			volumeSerial: fileID.VolumeSerialNumber,
			fileID:       fileID.FileID,
		},
	}, nil
}
