//go:build windows

package lock

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestRootIdentity_UsesRequiredOpenAndFileIDParameters(t *testing.T) {
	api := newTestWindowsAPI()
	resource, err := openRootIdentity(t.Context(), api, `C:\app`)
	if err != nil {
		t.Fatalf("openRootIdentity() error = %v", err)
	}
	if resource.handle != testRootHandle {
		t.Fatalf(
			"root handle = %#x, want %#x",
			resource.handle,
			testRootHandle,
		)
	}
	if resource.identity.volumeSerial != api.fileID.VolumeSerialNumber ||
		resource.identity.fileID != api.fileID.FileID {
		t.Fatalf(
			"identity = %#v, want serial/file ID %#v",
			resource.identity,
			api.fileID,
		)
	}

	openCalls := api.callsFor("create-file")
	if len(openCalls) != 1 {
		t.Fatalf("CreateFile calls = %d, want 1", len(openCalls))
	}
	open := openCalls[0]
	if open.Access != windows.FILE_READ_ATTRIBUTES {
		t.Fatalf("access = %#x, want FILE_READ_ATTRIBUTES", open.Access)
	}
	wantShare := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if open.Share != wantShare {
		t.Fatalf("share = %#x, want %#x", open.Share, wantShare)
	}
	if open.Creation != windows.OPEN_EXISTING {
		t.Fatalf("creation = %#x, want OPEN_EXISTING", open.Creation)
	}
	if open.Flags != windows.FILE_FLAG_BACKUP_SEMANTICS {
		t.Fatalf(
			"flags = %#x, want FILE_FLAG_BACKUP_SEMANTICS",
			open.Flags,
		)
	}
	if !open.SecurityWasNil || open.Handle != 0 {
		t.Fatalf(
			"security nil/template = %t/%#x, want true/0",
			open.SecurityWasNil,
			open.Handle,
		)
	}

	fileIDCalls := api.callsFor("file-id")
	if len(fileIDCalls) != 1 {
		t.Fatalf("FileIdInfo calls = %d, want 1", len(fileIDCalls))
	}
	fileID := fileIDCalls[0]
	if fileID.Handle != testRootHandle ||
		fileID.Class != windows.FileIdInfo ||
		fileID.Size != uint32(unsafe.Sizeof(fileIDInfo{})) {
		t.Fatalf(
			"FileIdInfo handle/class/size = %#x/%d/%d",
			fileID.Handle,
			fileID.Class,
			fileID.Size,
		)
	}
	if err := api.closeHandle(resource.handle); err != nil {
		t.Fatalf("close root handle error = %v", err)
	}
}

func TestRootIdentity_RejectsMissingFileAndNonDirectory(t *testing.T) {
	tests := []struct {
		name string
		api  func() *testWindowsAPI
	}{
		{
			name: "missing",
			api: func() *testWindowsAPI {
				api := newTestWindowsAPI()
				api.createFileErr = windows.ERROR_FILE_NOT_FOUND
				return api
			},
		},
		{
			name: "regular file",
			api: func() *testWindowsAPI {
				api := newTestWindowsAPI()
				api.fileAttributes = windows.FILE_ATTRIBUTE_NORMAL
				return api
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := test.api()
			_, err := openRootIdentity(t.Context(), api, `C:\app`)
			if err == nil {
				t.Fatal("openRootIdentity() error = nil, want rejection")
			}
			if test.name == "regular file" && api.count("close") != 1 {
				t.Fatalf(
					"CloseHandle calls = %d, want 1",
					api.count("close"),
				)
			}
			if test.name == "missing" &&
				!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				t.Fatalf("error = %v, want ERROR_FILE_NOT_FOUND", err)
			}
		})
	}
}
