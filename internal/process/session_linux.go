package process

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func sessionMembers(session int) ([]Info, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := make([]Info, 0)
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid == session {
			continue
		}
		data, e := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, unix.ESRCH) {
			continue
		}
		if e != nil {
			return nil, e
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return nil, errors.New("invalid process stat")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 4 {
			return nil, errors.New("invalid process stat fields")
		}
		sid, e := strconv.Atoi(fields[3])
		if e != nil {
			return nil, e
		}
		if sid != session || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		parent, e := strconv.ParseUint(fields[1], 10, 32)
		if e != nil {
			return nil, e
		}
		name, e := os.Readlink("/proc/" + entry.Name() + "/exe")
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, unix.ESRCH) {
			continue
		}
		if e != nil {
			// 路径读取仅用于诊断；已确认的 session 成员必须保留以便回收。
			name = ""
		}

		result = append(result, Info{PID: uint32(pid), ParentPID: uint32(parent), Executable: name})
	}
	return result, nil
}
