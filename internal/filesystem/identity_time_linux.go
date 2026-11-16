//go:build linux

package filesystem

import "golang.org/x/sys/unix"

// posixStatChangeTime 返回 Linux inode 状态变化时间。
func posixStatChangeTime(st *unix.Stat_t) (int64, int32) {
	return st.Ctim.Sec, int32(st.Ctim.Nsec)
}

// posixStatGeneration 返回 Linux 可用的 inode 代际；当前 ABI 不提供该字段。
func posixStatGeneration(*unix.Stat_t) uint32 {
	return 0
}
