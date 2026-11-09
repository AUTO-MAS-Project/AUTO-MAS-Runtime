package config

import "path/filepath"

// DarwinLockPaths 是 macOS 运行时使用的三个持久锁文件。
type DarwinLockPaths struct {
	Backend     string
	Mutation    string
	Coordinator string
}

// DarwinLockPaths 返回状态目录内的固定路径，不分配随机名称。
func (l *Layout) DarwinLockPaths() DarwinLockPaths {
	return DarwinLockPaths{
		Backend:     filepath.Join(l.StateDir(), "backend.lock"),
		Mutation:    filepath.Join(l.StateDir(), "mutation.lock"),
		Coordinator: filepath.Join(l.StateDir(), "lock-coordinator.lock"),
	}
}
