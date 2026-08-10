//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func waitTestProcessExit(pid int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("process %d did not exit within %s", pid, timeout)
		case <-ticker.C:
		}
	}
}

func terminateTestProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
