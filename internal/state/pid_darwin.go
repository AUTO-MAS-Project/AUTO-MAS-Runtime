package state

import (
	"time"

	"golang.org/x/sys/unix"
)

func posixCreatedAt(pid uint32) (time.Time, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(info.Proc.P_starttime.Sec, int64(info.Proc.P_starttime.Usec)*1000).UTC(), nil
}

func posixIdentityToken(pid uint32) (string, error) {
	created, err := posixCreatedAt(pid)
	if err != nil {
		return "", err
	}
	return created.Format(time.RFC3339Nano), nil
}
