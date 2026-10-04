package filesystem

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestIdentityWindows_ExFATFallback(t *testing.T) {
	legacyErr := errors.New("legacy query failed")
	volumeErr := errors.New("volume query failed")
	tests := []struct {
		name      string
		idErr     error
		format    string
		volumeErr error
		legacyErr error
		mutate    func(*windows.ByHandleFileInformation)
		wantErr   error
		wantCalls int
	}{
		{name: "extended", wantCalls: 0},
		{name: "invalid_parameter", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", wantCalls: 1},
		{name: "not_supported", idErr: windows.ERROR_NOT_SUPPORTED, format: "EXFAT", wantCalls: 1},
		{name: "invalid_function", idErr: windows.ERROR_INVALID_FUNCTION, format: "exFAT", wantCalls: 1},
		{name: "access_denied", idErr: windows.ERROR_ACCESS_DENIED, format: "exFAT", wantErr: windows.ERROR_ACCESS_DENIED},
		{name: "invalid_handle", idErr: windows.ERROR_INVALID_HANDLE, format: "exFAT", wantErr: windows.ERROR_INVALID_HANDLE},
		{name: "ntfs", idErr: windows.ERROR_INVALID_PARAMETER, format: "NTFS", wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "refs", idErr: windows.ERROR_INVALID_PARAMETER, format: "ReFS", wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "fat32", idErr: windows.ERROR_INVALID_PARAMETER, format: "FAT32", wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "unknown", idErr: windows.ERROR_INVALID_PARAMETER, wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "volume_error", idErr: windows.ERROR_INVALID_PARAMETER, volumeErr: volumeErr, wantErr: volumeErr},
		{name: "legacy_error", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", legacyErr: legacyErr, wantErr: legacyErr, wantCalls: 1},
		{name: "zero_id", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", mutate: func(i *windows.ByHandleFileInformation) { i.FileIndexHigh, i.FileIndexLow = 0, 0 }, wantErr: ErrIdentityChanged, wantCalls: 1},
		{name: "attributes_changed", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", mutate: func(i *windows.ByHandleFileInformation) { i.FileAttributes = windows.FILE_ATTRIBUTE_REPARSE_POINT }, wantErr: ErrIdentityChanged, wantCalls: 1},
		{name: "links_changed", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", mutate: func(i *windows.ByHandleFileInformation) { i.NumberOfLinks = 2 }, wantErr: ErrIdentityChanged, wantCalls: 1},
		{name: "size_changed", idErr: windows.ERROR_INVALID_PARAMETER, format: "exFAT", mutate: func(i *windows.ByHandleFileInformation) { i.FileSizeLow++ }, wantErr: ErrIdentityChanged, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const handle windows.Handle = 42
			calls, volumeCalls := 0, 0
			api := identityAPI{
				information: func(h windows.Handle, class uint32, p unsafe.Pointer, size uintptr) error {
					if h != handle {
						t.Fatalf("got handle %v, want %v", h, handle)
					}
					switch class {
					case fileBasicInfoClass:
						if size != unsafe.Sizeof(fileBasicInfo{}) {
							t.Fatalf("got basic size %d", size)
						}
						*(*fileBasicInfo)(p) = fileBasicInfo{attributes: windows.FILE_ATTRIBUTE_NORMAL}
					case fileStandardInfoClass:
						*(*fileStandardInfo)(p) = fileStandardInfo{numberOfLinks: 1, endOfFile: 0x100000007}
					case fileIDInfoClass:
						*(*fileIDInfo)(p) = fileIDInfo{volumeSerial: 0x123456789abcdef0, fileID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}
						return test.idErr
					default:
						t.Fatalf("unexpected information class %d", class)
					}
					return nil
				},
				legacy: func(h windows.Handle, i *windows.ByHandleFileInformation) error {
					if h != handle {
						t.Fatalf("got legacy handle %v, want %v", h, handle)
					}
					calls++
					*i = windows.ByHandleFileInformation{VolumeSerialNumber: 0xfedcba98, FileIndexHigh: 0x12345678, FileIndexLow: 0x9abcdef0, FileAttributes: windows.FILE_ATTRIBUTE_NORMAL, NumberOfLinks: 1, FileSizeHigh: 1, FileSizeLow: 7}
					if test.mutate != nil {
						test.mutate(i)
					}
					return test.legacyErr
				},
				volume: func(h windows.Handle) (string, uint32, error) {
					if h != handle {
						t.Fatalf("got volume handle %v, want %v", h, handle)
					}
					volumeCalls++
					return test.format, 0, test.volumeErr
				},
			}
			got, err := identityWindowsWith(handle, api)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || got != (objectIdentity{}) {
					t.Fatalf("got %#v, %v, want zero identity and %v", got, err, test.wantErr)
				}
				if !errors.Is(err, test.idErr) {
					t.Fatalf("got %v, want original error %v", err, test.idErr)
				}
			} else {
				want := objectIdentity{volumeSerial: 0x123456789abcdef0, fileID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, attributes: windows.FILE_ATTRIBUTE_NORMAL, numberOfLinks: 1, size: 0x100000007}
				if test.idErr != nil {
					want.volumeSerial = 0xfedcba98
					want.fileID = [16]byte{}
					binary.LittleEndian.PutUint64(want.fileID[:8], 0x123456789abcdef0)
				}
				if err != nil || got != want {
					t.Fatalf("got %#v, %v, want %#v", got, err, want)
				}
			}
			if calls != test.wantCalls {
				t.Fatalf("got legacy calls %d, want %d", calls, test.wantCalls)
			}
			if (test.idErr == nil || errors.Is(test.idErr, windows.ERROR_ACCESS_DENIED) || errors.Is(test.idErr, windows.ERROR_INVALID_HANDLE)) && volumeCalls != 0 {
				t.Fatalf("got volume calls %d, want 0", volumeCalls)
			}
		})
	}
}

func TestCaseSensitiveWindows_ExFATFallback(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		format  string
		flags   uint32
		want    bool
		wantErr error
	}{
		{name: "supported_sensitive", want: true},
		{name: "exfat_unsupported", err: windows.ERROR_INVALID_PARAMETER, format: "exFAT"},
		{name: "exfat_not_supported", err: windows.ERROR_NOT_SUPPORTED, format: "EXFAT"},
		{name: "exfat_invalid_function", err: windows.ERROR_INVALID_FUNCTION, format: "exFAT"},
		{name: "exfat_sensitive_capability", err: windows.ERROR_INVALID_PARAMETER, format: "exFAT", flags: windows.FILE_CASE_SENSITIVE_SEARCH, wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "ntfs", err: windows.ERROR_INVALID_PARAMETER, format: "NTFS", wantErr: windows.ERROR_INVALID_PARAMETER},
		{name: "permission", err: windows.ERROR_ACCESS_DENIED, format: "exFAT", wantErr: windows.ERROR_ACCESS_DENIED},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := identityAPI{
				information: func(_ windows.Handle, class uint32, p unsafe.Pointer, _ uintptr) error {
					if class != fileCaseSensitiveClass {
						t.Fatalf("got class %d, want case-sensitive class", class)
					}
					*(*fileCaseSensitiveInfo)(p) = fileCaseSensitiveInfo{flags: fileCaseSensitiveDir}
					return test.err
				},
				volume: func(windows.Handle) (string, uint32, error) { return test.format, test.flags, nil },
			}
			got, err := caseSensitiveWindowsWith(42, api)
			if got != test.want || (test.wantErr == nil && err != nil) || (test.wantErr != nil && !errors.Is(err, test.wantErr)) {
				t.Fatalf("got %v, %v, want %v, %v", got, err, test.want, test.wantErr)
			}
		})
	}
}

func simulatedExFATIdentityAPI() identityAPI {
	api := newIdentityAPI()
	query := api.information
	api.information = func(handle windows.Handle, class uint32, p unsafe.Pointer, size uintptr) error {
		if class == fileIDInfoClass || class == fileCaseSensitiveClass {
			return windows.ERROR_INVALID_PARAMETER
		}
		return query(handle, class, p, size)
	}
	api.volume = func(windows.Handle) (string, uint32, error) { return "exFAT", 0, nil }
	return api
}

func TestIdentityWindows_ExFATHandleLifecycle(t *testing.T) {
	api := simulatedExFATIdentityAPI()
	root := t.TempDir()
	path := filepath.Join(root, "object")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := openSpec{access: windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, share: windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE, creation: windows.OPEN_EXISTING, options: windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT, directory: true}
	open := func(path string) windows.Handle {
		t.Helper()
		h, err := openPathWindows(path, spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := windows.CloseHandle(h); err != nil {
				t.Errorf("close identity handle: %v", err)
			}
		})
		return h
	}
	identify := func(h windows.Handle) objectIdentity {
		t.Helper()
		id, err := identityWindowsWith(h, api)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	h := open(path)
	if format, _, err := volumeInformationWindows(h); err != nil || format == "" {
		t.Fatalf("got real handle filesystem %q, %v, want a known filesystem", format, err)
	}
	before := identify(h)
	reopened := identify(open(path))
	if !before.Equal(&reopened) {
		t.Fatalf("got reopened %#v, want %#v", reopened, before)
	}
	longPath := filepath.Join(root, "object-with-a-much-longer-name")
	if err := os.Rename(path, longPath); err != nil {
		t.Fatal(err)
	}
	after := identify(h)
	if !before.Equal(&after) {
		t.Fatalf("got renamed %#v, want %#v", after, before)
	}
	after = identify(open(longPath))
	if !before.Equal(&after) {
		t.Fatalf("got reopened rename %#v, want %#v", after, before)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := identify(open(path))
	if before.Equal(&replacement) {
		t.Fatalf("got replacement %#v identical to pinned old object", replacement)
	}
}

func TestIdentityWindows_ExFATVolumeLifecycle(t *testing.T) {
	parent := os.Getenv("AUTO_MAS_TEST_EXFAT_ROOT")
	if parent == "" {
		t.Skip("AUTO_MAS_TEST_EXFAT_ROOT is not set; real exFAT validation remains outstanding")
	}
	if !filepath.IsAbs(parent) {
		t.Fatal("AUTO_MAS_TEST_EXFAT_ROOT must be absolute")
	}
	root, err := os.MkdirTemp(parent, "auto-mas-exfat-test-")
	if err != nil {
		t.Fatal(err)
	}
	// 仅清理本测试在显式父目录下创建的随机目录，拒绝递归删除外部路径或重解析点。
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil {
			t.Errorf("remove empty test root: %v", err)
		}
	})
	path := filepath.Join(root, "object")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(path); err != nil {
			t.Errorf("remove test object: %v", err)
		}
	}()
	handle, err := openPathWindows(path, openSpec{access: windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, share: windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE, creation: windows.OPEN_EXISTING, options: windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT, directory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Errorf("close object: %v", err)
		}
	}()
	format, _, err := volumeInformationWindows(handle)
	if err != nil || format != "exFAT" {
		t.Fatalf("got filesystem %q, %v, want exFAT", format, err)
	}
	before, err := identityWindows(handle)
	if err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(root, "other-object")
	if err := os.Mkdir(otherPath, 0o700); err != nil {
		t.Fatal(err)
	}
	otherHandle, err := openPathWindows(otherPath, directoryPinSpec())
	if err != nil {
		t.Fatal(err)
	}
	other, identityErr := identityWindows(otherHandle)
	closeErr := windows.CloseHandle(otherHandle)
	removeErr := os.Remove(otherPath)
	if identityErr != nil || closeErr != nil || removeErr != nil || before.Equal(&other) {
		t.Fatalf("distinct object = %#v, %v, %v, %v, want different from %#v", other, identityErr, closeErr, removeErr, before)
	}
	longPath := filepath.Join(root, "object-with-a-much-longer-name")
	if err := os.Rename(path, longPath); err != nil {
		t.Fatal(err)
	}
	path = longPath
	after, err := identityWindows(handle)
	if err != nil || !before.Equal(&after) {
		t.Fatalf("identity after rename = %#v, %v, want %#v", after, err, before)
	}
	verifyIdentityAtPath(t, path, before)
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "child")); err != nil {
		t.Fatal(err)
	}
	verifyIdentityAtPath(t, path, before)
	// 同一真实卷上再验状态生命周期，只清理随机测试布局。
	verifyExFATStateLifecycle(t, root, newProductionPathAPI())
}

func verifyIdentityAtPath(t *testing.T, path string, want objectIdentity) {
	t.Helper()
	handle, err := openPathWindows(path, directoryPinSpec())
	if err != nil {
		t.Fatal(err)
	}
	got, identifyErr := identityWindows(handle)
	closeErr := windows.CloseHandle(handle)
	if identifyErr != nil || closeErr != nil || !want.Equal(&got) {
		t.Fatalf("got identity %#v, %v, %v, want %#v", got, identifyErr, closeErr, want)
	}
}
