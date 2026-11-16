package process_test

import "golang.org/x/sys/unix"

func zombiePOSIX(pid int) bool {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && info.Proc.P_stat == 5
}
