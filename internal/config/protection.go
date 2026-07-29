package config

// ProtectedRootDirs 返回自动流程不得删除或移动的根级目录。
func (l *Layout) ProtectedRootDirs() []string {
	return []string{
		l.paths.configDir,
		l.paths.dataDir,
		l.paths.historyDir,
		l.paths.scriptDir,
		l.paths.debugDir,
		l.paths.pluginsDir,
		l.paths.logsDir,
	}
}
