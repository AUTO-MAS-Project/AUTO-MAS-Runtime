package config

import "path/filepath"

type layoutPaths struct {
	repoDir              string
	repoVersionFile      string
	stateDir             string
	backendStateFile     string
	mutationStateFile    string
	updateStateFile      string
	environmentStateFile string
	runtimeDir           string
	uvToolsDir           string
	pythonDir            string
	venvDir              string
	runtimeCacheDir      string
	uvCacheDir           string
	downloadCacheDir     string
	buildCacheDir        string
	logsDir              string
	runtimeLogDir        string
	configDir            string
	dataDir              string
	historyDir           string
	scriptDir            string
	debugDir             string
	pluginsDir           string
}

func newLayoutPaths(root string) layoutPaths {
	stateDir := filepath.Join(root, "runtime-state")
	runtimeDir := filepath.Join(root, "runtime")
	environmentDir := filepath.Join(runtimeDir, "environment")
	cacheDir := filepath.Join(runtimeDir, "cache")
	logsDir := filepath.Join(root, "logs")

	return layoutPaths{
		repoDir:              filepath.Join(root, "repo"),
		repoVersionFile:      filepath.Join(root, "repo", "res", "version.json"),
		stateDir:             stateDir,
		backendStateFile:     filepath.Join(stateDir, "backend.json"),
		mutationStateFile:    filepath.Join(stateDir, "mutation.json"),
		updateStateFile:      filepath.Join(stateDir, "update.json"),
		environmentStateFile: filepath.Join(stateDir, "environment.json"),
		runtimeDir:           runtimeDir,
		uvToolsDir:           filepath.Join(runtimeDir, "tools", "uv"),
		pythonDir:            filepath.Join(environmentDir, "python"),
		venvDir:              filepath.Join(environmentDir, "venv"),
		runtimeCacheDir:      cacheDir,
		uvCacheDir:           filepath.Join(cacheDir, "uv"),
		downloadCacheDir:     filepath.Join(cacheDir, "downloads"),
		buildCacheDir:        filepath.Join(cacheDir, "build"),
		logsDir:              logsDir,
		runtimeLogDir:        filepath.Join(logsDir, "runtime"),
		configDir:            filepath.Join(root, "config"),
		dataDir:              filepath.Join(root, "data"),
		historyDir:           filepath.Join(root, "history"),
		scriptDir:            filepath.Join(root, "script"),
		debugDir:             filepath.Join(root, "debug"),
		pluginsDir:           filepath.Join(root, "plugins"),
	}
}

// RepoDir 返回受管后端仓库目录。
func (l *Layout) RepoDir() string { return l.paths.repoDir }

// RepoVersionFile 返回受管仓库版本文件路径。
func (l *Layout) RepoVersionFile() string { return l.paths.repoVersionFile }

// StateDir 返回 Runtime 状态文件目录。
func (l *Layout) StateDir() string { return l.paths.stateDir }

// BackendStateFile 返回后端状态文件路径。
func (l *Layout) BackendStateFile() string { return l.paths.backendStateFile }

// MutationStateFile 返回变更事务状态文件路径。
func (l *Layout) MutationStateFile() string { return l.paths.mutationStateFile }

// UpdateStateFile 返回更新状态文件路径。
func (l *Layout) UpdateStateFile() string { return l.paths.updateStateFile }

// EnvironmentStateFile 返回环境状态文件路径。
func (l *Layout) EnvironmentStateFile() string { return l.paths.environmentStateFile }

// RuntimeDir 返回 Runtime 受管运行目录。
func (l *Layout) RuntimeDir() string { return l.paths.runtimeDir }

// UVToolsDir 返回受管 uv 工具目录。
func (l *Layout) UVToolsDir() string { return l.paths.uvToolsDir }

// PythonDir 返回受管 Python 目录。
func (l *Layout) PythonDir() string { return l.paths.pythonDir }

// VenvDir 返回受管 Python 虚拟环境目录。
func (l *Layout) VenvDir() string { return l.paths.venvDir }

// RuntimeCacheDir 返回 Runtime 缓存根目录。
func (l *Layout) RuntimeCacheDir() string { return l.paths.runtimeCacheDir }

// UVCacheDir 返回 uv 缓存目录。
func (l *Layout) UVCacheDir() string { return l.paths.uvCacheDir }

// DownloadCacheDir 返回下载缓存目录。
func (l *Layout) DownloadCacheDir() string { return l.paths.downloadCacheDir }

// BuildCacheDir 返回构建缓存目录。
func (l *Layout) BuildCacheDir() string { return l.paths.buildCacheDir }

// LogsDir 返回日志根目录。
func (l *Layout) LogsDir() string { return l.paths.logsDir }

// RuntimeLogDir 返回 Runtime 日志目录。
func (l *Layout) RuntimeLogDir() string { return l.paths.runtimeLogDir }

// ConfigDir 返回用户配置目录。
func (l *Layout) ConfigDir() string { return l.paths.configDir }

// DataDir 返回用户数据目录。
func (l *Layout) DataDir() string { return l.paths.dataDir }

// HistoryDir 返回历史记录目录。
func (l *Layout) HistoryDir() string { return l.paths.historyDir }

// ScriptDir 返回外部脚本目录。
func (l *Layout) ScriptDir() string { return l.paths.scriptDir }

// DebugDir 返回诊断数据目录。
func (l *Layout) DebugDir() string { return l.paths.debugDir }

// PluginsDir 返回插件目录。
func (l *Layout) PluginsDir() string { return l.paths.pluginsDir }
