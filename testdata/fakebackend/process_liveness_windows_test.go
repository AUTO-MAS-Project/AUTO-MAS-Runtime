//go:build windows

package main

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func waitTestProcessExit(pid int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	deadline := uint32(timeout / time.Millisecond)
	result, err := windows.WaitForSingleObject(handle, deadline)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d did not exit within %s", pid, timeout)
	}
	return nil
}

func terminateTestProcess(pid int) error {
	handle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.TerminateProcess(handle, 99); err != nil {
		return err
	}
	return nil
}
