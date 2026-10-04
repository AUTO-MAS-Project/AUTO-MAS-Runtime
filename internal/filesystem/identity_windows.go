package filesystem

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type identityAPI struct {
	information func(windows.Handle, uint32, unsafe.Pointer, uintptr) error
	legacy      func(windows.Handle, *windows.ByHandleFileInformation) error
	volume      func(windows.Handle) (string, uint32, error)
}

func newIdentityAPI() identityAPI {
	return identityAPI{
		information: getFileInformationWindows,
		legacy:      windows.GetFileInformationByHandle,
		volume:      volumeInformationWindows,
	}
}

func volumeInformationWindows(handle windows.Handle) (string, uint32, error) {
	var name [windows.MAX_PATH + 1]uint16
	var flags uint32
	err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, &name[0], uint32(len(name)))
	if err != nil {
		return "", 0, err
	}
	return windows.UTF16ToString(name[:]), flags, nil
}

func isUnsupportedFileInformation(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

func exFATVolumeWith(handle windows.Handle, queryErr error, api identityAPI) (uint32, error) {
	if !isUnsupportedFileInformation(queryErr) {
		return 0, queryErr
	}
	// 必须从正在校验的句柄识别卷，路径查询会重新引入同名替换窗口。
	format, flags, err := api.volume(handle)
	if err != nil {
		return 0, errors.Join(queryErr, fmt.Errorf("read volume information: %w", err))
	}
	if !strings.EqualFold(format, "exFAT") {
		return 0, queryErr
	}
	return flags, nil
}

func legacyExFATIdentityWith(
	handle windows.Handle,
	basic fileBasicInfo,
	standard fileStandardInfo,
	queryErr error,
	api identityAPI,
) (fileIDInfo, error) {
	if _, err := exFATVolumeWith(handle, queryErr, api); err != nil {
		return fileIDInfo{}, err
	}
	var legacy windows.ByHandleFileInformation
	if err := api.legacy(handle, &legacy); err != nil {
		return fileIDInfo{}, errors.Join(queryErr, fmt.Errorf("read legacy file information: %w", err))
	}
	index := uint64(legacy.FileIndexHigh)<<32 | uint64(legacy.FileIndexLow)
	size := uint64(legacy.FileSizeHigh)<<32 | uint64(legacy.FileSizeLow)
	if index == 0 || legacy.FileAttributes != basic.attributes ||
		legacy.NumberOfLinks != standard.numberOfLinks || standard.endOfFile < 0 ||
		size != uint64(standard.endOfFile) {
		return fileIDInfo{}, errors.Join(queryErr, ErrIdentityChanged)
	}
	// 仅 exFAT 使用旧版索引；NTFS/ReFS 的 128 位身份绝不能被截断。
	id := fileIDInfo{volumeSerial: uint64(legacy.VolumeSerialNumber)}
	binary.LittleEndian.PutUint64(id.fileID[:8], index)
	return id, nil
}
