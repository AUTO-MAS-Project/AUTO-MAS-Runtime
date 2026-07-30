//go:build windows

package lock

import "golang.org/x/sys/windows"

type windowsAPI interface {
	createFile(
		name *uint16,
		access uint32,
		share uint32,
		security *windows.SecurityAttributes,
		creation uint32,
		flags uint32,
		template windows.Handle,
	) (windows.Handle, error)
	getFileInformationByHandle(
		handle windows.Handle,
		information *windows.ByHandleFileInformation,
	) error
	getFileInformationByHandleEx(
		handle windows.Handle,
		class uint32,
		information *byte,
		size uint32,
	) error
	currentProcess() windows.Handle
	currentThread() windows.Handle
	duplicateHandle(
		sourceProcess windows.Handle,
		source windows.Handle,
		targetProcess windows.Handle,
		target *windows.Handle,
		desiredAccess uint32,
		inheritHandle bool,
		options uint32,
	) error
	createMutex(
		security *windows.SecurityAttributes,
		initialOwner bool,
		name *uint16,
	) (windows.Handle, error)
	waitForSingleObject(
		handle windows.Handle,
		timeoutMilliseconds uint32,
	) (uint32, error)
	releaseMutex(handle windows.Handle) error
	closeHandle(handle windows.Handle) error
}

type systemWindowsAPI struct{}

func (systemWindowsAPI) createFile(
	name *uint16,
	access uint32,
	share uint32,
	security *windows.SecurityAttributes,
	creation uint32,
	flags uint32,
	template windows.Handle,
) (windows.Handle, error) {
	return windows.CreateFile(
		name,
		access,
		share,
		security,
		creation,
		flags,
		template,
	)
}

func (systemWindowsAPI) getFileInformationByHandle(
	handle windows.Handle,
	information *windows.ByHandleFileInformation,
) error {
	return windows.GetFileInformationByHandle(handle, information)
}

func (systemWindowsAPI) getFileInformationByHandleEx(
	handle windows.Handle,
	class uint32,
	information *byte,
	size uint32,
) error {
	return windows.GetFileInformationByHandleEx(
		handle,
		class,
		information,
		size,
	)
}

func (systemWindowsAPI) currentProcess() windows.Handle {
	return windows.CurrentProcess()
}

func (systemWindowsAPI) currentThread() windows.Handle {
	return windows.CurrentThread()
}

func (systemWindowsAPI) duplicateHandle(
	sourceProcess windows.Handle,
	source windows.Handle,
	targetProcess windows.Handle,
	target *windows.Handle,
	desiredAccess uint32,
	inheritHandle bool,
	options uint32,
) error {
	return windows.DuplicateHandle(
		sourceProcess,
		source,
		targetProcess,
		target,
		desiredAccess,
		inheritHandle,
		options,
	)
}

func (systemWindowsAPI) createMutex(
	security *windows.SecurityAttributes,
	initialOwner bool,
	name *uint16,
) (windows.Handle, error) {
	return windows.CreateMutex(security, initialOwner, name)
}

func (systemWindowsAPI) waitForSingleObject(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (uint32, error) {
	return windows.WaitForSingleObject(handle, timeoutMilliseconds)
}

func (systemWindowsAPI) releaseMutex(handle windows.Handle) error {
	return windows.ReleaseMutex(handle)
}

func (systemWindowsAPI) closeHandle(handle windows.Handle) error {
	return windows.CloseHandle(handle)
}
