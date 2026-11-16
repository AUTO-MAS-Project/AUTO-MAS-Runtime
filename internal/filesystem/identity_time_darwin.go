//go:build darwin

package filesystem

import "golang.org/x/sys/unix"

// posixStatChangeTime 返回 Darwin inode 状态变化时间。
func posixStatChangeTime(st *unix.Stat_t) (int64, int32) {
	return st.Ctim.Sec, int32(st.Ctim.Nsec)
}

// posixStatGeneration 返回 Darwin inode generation。
func posixStatGeneration(st *unix.Stat_t) uint32 {
	return st.Gen
}
