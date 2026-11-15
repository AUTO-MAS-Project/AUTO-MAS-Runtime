package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func linuxStartTicks(pid uint32) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm 可含空格和括号，字段只能从最后一个右括号之后切分。
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, ErrInvalidPID
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, ErrInvalidPID
	}
	return strconv.ParseInt(fields[19], 10, 64)
}
func posixCreatedAt(pid uint32) (time.Time, error) {
	ticks, err := linuxStartTicks(pid)
	if err != nil {
		return time.Time{}, err
	}
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			boot, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			// Linux /proc ABI 的 USER_HZ 在 amd64/arm64 均为 100，与内核调度 HZ 无关。
			return time.Unix(boot+ticks/100, (ticks%100)*int64(time.Second/100)).UTC(), nil
		}
	}
	return time.Time{}, ErrInvalidPID
}
func posixIdentityToken(pid uint32) (string, error) {
	ticks, err := linuxStartTicks(pid)
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + strconv.FormatInt(ticks, 10), nil
}
