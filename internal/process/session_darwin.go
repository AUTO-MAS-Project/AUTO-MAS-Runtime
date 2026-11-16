package process

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

func sessionMembers(session int) ([]Info, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	result := make([]Info, 0)
	for _, entry := range entries {
		pid := int(entry.Proc.P_pid)
		if pid <= 0 || pid == session || entry.Proc.P_stat == 5 {
			continue
		}
		sid, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if sid != session {
			continue
		}
		args, pathErr := unix.SysctlRaw("kern.procargs2", pid)
		if errors.Is(pathErr, unix.ESRCH) {
			continue
		}
		name := ""
		if pathErr == nil && len(args) >= 5 {
			if end := bytes.IndexByte(args[4:], 0); end >= 0 {
				name = string(args[4 : 4+end])
			}
		}

		result = append(result, Info{PID: uint32(pid), ParentPID: uint32(entry.Eproc.Ppid), Executable: name})
	}
	return result, nil
}
