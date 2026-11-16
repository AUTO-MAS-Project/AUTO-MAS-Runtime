package process

// RunInternalGuardian 在 Windows 上不启用内部 POSIX 模式。
func RunInternalGuardian() (bool, int) { return false, 0 }
