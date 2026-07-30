//go:build windows

package lock

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	testProcessHandle  windows.Handle = 0x101
	testThreadPseudo   windows.Handle = 0x102
	testThreadHandle   windows.Handle = 0x103
	testRootHandle     windows.Handle = 0x104
	testBackendHandle  windows.Handle = 0x105
	testMutationHandle windows.Handle = 0x106
)

type apiCall struct {
	Operation      string
	Index          int
	ThreadID       uint32
	Handle         windows.Handle
	Timeout        uint32
	Access         uint32
	Share          uint32
	Creation       uint32
	Flags          uint32
	Class          uint32
	Size           uint32
	DesiredAccess  uint32
	InheritHandle  bool
	Options        uint32
	InitialOwner   bool
	SecurityWasNil bool
	Name           string
}

type testWindowsAPI struct {
	mu sync.Mutex

	calls  []apiCall
	counts map[string]int

	fileAttributes uint32
	fileID         fileIDInfo

	createFileErr   error
	inspectRootErr  error
	fileIDErr       error
	duplicateErr    error
	createMutexErr  map[int]error
	zeroMutexHandle map[int]bool

	waitResult       func(apiCall) (uint32, error)
	threadWaitResult func(apiCall) (uint32, error)
	releaseErr       func(apiCall) error
	closeErr         func(apiCall) error
	beforeCall       func(apiCall)
	afterCall        func(apiCall)
}

func newTestWindowsAPI() *testWindowsAPI {
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(i)
	}
	return &testWindowsAPI{
		counts:         make(map[string]int),
		fileAttributes: windows.FILE_ATTRIBUTE_DIRECTORY,
		fileID: fileIDInfo{
			VolumeSerialNumber: 0x0102030405060708,
			FileID:             fileID,
		},
		createMutexErr:  make(map[int]error),
		zeroMutexHandle: make(map[int]bool),
	}
}

func (a *testWindowsAPI) record(call apiCall) apiCall {
	a.mu.Lock()
	if a.counts == nil {
		a.counts = make(map[string]int)
	}
	a.counts[call.Operation]++
	call.Index = a.counts[call.Operation]
	call.ThreadID = windows.GetCurrentThreadId()
	a.calls = append(a.calls, call)
	before := a.beforeCall
	a.mu.Unlock()
	if before != nil {
		before(call)
	}
	return call
}

func (a *testWindowsAPI) finish(call apiCall) {
	a.mu.Lock()
	after := a.afterCall
	a.mu.Unlock()
	if after != nil {
		after(call)
	}
}

func (a *testWindowsAPI) count(operation string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[operation]
}

func (a *testWindowsAPI) callsFor(operation string) []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []apiCall
	for _, call := range a.calls {
		if call.Operation == operation {
			result = append(result, call)
		}
	}
	return result
}

func (a *testWindowsAPI) callSequence() []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]apiCall(nil), a.calls...)
}

func (a *testWindowsAPI) createFile(
	name *uint16,
	access uint32,
	share uint32,
	security *windows.SecurityAttributes,
	creation uint32,
	flags uint32,
	template windows.Handle,
) (windows.Handle, error) {
	call := a.record(apiCall{
		Operation:      "create-file",
		Access:         access,
		Share:          share,
		Creation:       creation,
		Flags:          flags,
		Handle:         template,
		SecurityWasNil: security == nil,
		Name:           windows.UTF16PtrToString(name),
	})
	defer a.finish(call)
	if a.createFileErr != nil {
		return 0, a.createFileErr
	}
	return testRootHandle, nil
}

func (a *testWindowsAPI) getFileInformationByHandle(
	handle windows.Handle,
	information *windows.ByHandleFileInformation,
) error {
	call := a.record(apiCall{
		Operation: "inspect-root",
		Handle:    handle,
	})
	defer a.finish(call)
	if a.inspectRootErr != nil {
		return a.inspectRootErr
	}
	information.FileAttributes = a.fileAttributes
	return nil
}

func (a *testWindowsAPI) getFileInformationByHandleEx(
	handle windows.Handle,
	class uint32,
	information *byte,
	size uint32,
) error {
	call := a.record(apiCall{
		Operation: "file-id",
		Handle:    handle,
		Class:     class,
		Size:      size,
	})
	defer a.finish(call)
	if a.fileIDErr != nil {
		return a.fileIDErr
	}
	*(*fileIDInfo)(unsafe.Pointer(information)) = a.fileID
	return nil
}

func (a *testWindowsAPI) currentProcess() windows.Handle {
	call := a.record(apiCall{Operation: "current-process"})
	defer a.finish(call)
	return testProcessHandle
}

func (a *testWindowsAPI) currentThread() windows.Handle {
	call := a.record(apiCall{Operation: "current-thread"})
	defer a.finish(call)
	return testThreadPseudo
}

func (a *testWindowsAPI) duplicateHandle(
	sourceProcess windows.Handle,
	source windows.Handle,
	targetProcess windows.Handle,
	target *windows.Handle,
	desiredAccess uint32,
	inheritHandle bool,
	options uint32,
) error {
	call := a.record(apiCall{
		Operation:     "duplicate-thread",
		Handle:        source,
		Access:        uint32(sourceProcess),
		Share:         uint32(targetProcess),
		DesiredAccess: desiredAccess,
		InheritHandle: inheritHandle,
		Options:       options,
	})
	defer a.finish(call)
	if a.duplicateErr != nil {
		return a.duplicateErr
	}
	*target = testThreadHandle
	return nil
}

func (a *testWindowsAPI) createMutex(
	security *windows.SecurityAttributes,
	initialOwner bool,
	name *uint16,
) (windows.Handle, error) {
	call := a.record(apiCall{
		Operation:      "create-mutex",
		InitialOwner:   initialOwner,
		SecurityWasNil: security == nil,
		Name:           windows.UTF16PtrToString(name),
	})
	defer a.finish(call)
	if err := a.createMutexErr[call.Index]; err != nil {
		return 0, err
	}
	if a.zeroMutexHandle[call.Index] {
		return 0, nil
	}
	if call.Index == 1 {
		return testBackendHandle, nil
	}
	return testMutationHandle, nil
}

func (a *testWindowsAPI) waitForSingleObject(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (uint32, error) {
	call := a.record(apiCall{
		Operation: "wait",
		Handle:    handle,
		Timeout:   timeoutMilliseconds,
	})
	defer a.finish(call)
	if handle == testThreadHandle {
		if a.threadWaitResult != nil {
			return a.threadWaitResult(call)
		}
		return waitResultObject0, nil
	}
	if a.waitResult != nil {
		return a.waitResult(call)
	}
	return waitResultObject0, nil
}

func (a *testWindowsAPI) releaseMutex(handle windows.Handle) error {
	call := a.record(apiCall{
		Operation: "release",
		Handle:    handle,
	})
	defer a.finish(call)
	if a.releaseErr != nil {
		return a.releaseErr(call)
	}
	return nil
}

func (a *testWindowsAPI) closeHandle(handle windows.Handle) error {
	call := a.record(apiCall{
		Operation: "close",
		Handle:    handle,
	})
	defer a.finish(call)
	if a.closeErr != nil {
		return a.closeErr(call)
	}
	return nil
}
