package protocol

import "slices"

// Stage 标识稳定的 Runtime 操作阶段。
type Stage string

const (
	StageRuntimeHandshake    Stage = "runtime.handshake"
	StageDoctor              Stage = "doctor"
	StageBootstrap           Stage = "bootstrap"
	StageRepair              Stage = "repair"
	StageCleanup             Stage = "cleanup"
	StageNetworkProbe        Stage = "network.probe"
	StageUVCheck             Stage = "uv.check"
	StageUVDownload          Stage = "uv.download"
	StageUVVerify            Stage = "uv.verify"
	StageWorkspaceCheck      Stage = "workspace.check"
	StageWorkspaceClone      Stage = "workspace.clone"
	StageWorkspaceVerify     Stage = "workspace.verify"
	StageWorkspaceSwap       Stage = "workspace.swap"
	StageWorkspaceCleanup    Stage = "workspace.cleanup"
	StagePythonCheck         Stage = "python.check"
	StagePythonInstall       Stage = "python.install"
	StageDependenciesCheck   Stage = "dependencies.check"
	StageDependenciesSync    Stage = "dependencies.sync"
	StageDependenciesRebuild Stage = "dependencies.rebuild"
	StageBackendSpawn        Stage = "backend.spawn"
	StageBackendHealth       Stage = "backend.health"
	StageBackendRun          Stage = "backend.run"
	StageBackendRestart      Stage = "backend.restart"
	StageBackendShutdown     Stage = "backend.shutdown"
	StageBackendCleanup      Stage = "backend.cleanup"
)

var stages = []Stage{
	StageRuntimeHandshake,
	StageDoctor,
	StageBootstrap,
	StageRepair,
	StageCleanup,
	StageNetworkProbe,
	StageUVCheck,
	StageUVDownload,
	StageUVVerify,
	StageWorkspaceCheck,
	StageWorkspaceClone,
	StageWorkspaceVerify,
	StageWorkspaceSwap,
	StageWorkspaceCleanup,
	StagePythonCheck,
	StagePythonInstall,
	StageDependenciesCheck,
	StageDependenciesSync,
	StageDependenciesRebuild,
	StageBackendSpawn,
	StageBackendHealth,
	StageBackendRun,
	StageBackendRestart,
	StageBackendShutdown,
	StageBackendCleanup,
}

// AllStages 按文档顺序返回全部稳定 stage 的防御性副本。
func AllStages() []Stage {
	return append([]Stage(nil), stages...)
}

// IsKnownStage 报告 value 是否为稳定的 Runtime stage。
func IsKnownStage(value Stage) bool {
	return slices.Contains(stages, value)
}

// ProgressStatus 标识一个 stage 的进度状态。
type ProgressStatus string

const (
	ProgressPending   ProgressStatus = "pending"
	ProgressRunning   ProgressStatus = "running"
	ProgressSucceeded ProgressStatus = "succeeded"
	ProgressSkipped   ProgressStatus = "skipped"
	ProgressFailed    ProgressStatus = "failed"
	ProgressCancelled ProgressStatus = "cancelled"
)

var progressStatuses = []ProgressStatus{
	ProgressPending,
	ProgressRunning,
	ProgressSucceeded,
	ProgressSkipped,
	ProgressFailed,
	ProgressCancelled,
}

// AllProgressStatuses 按文档顺序返回全部稳定进度状态的防御性副本。
func AllProgressStatuses() []ProgressStatus {
	return append([]ProgressStatus(nil), progressStatuses...)
}

// IsKnownProgressStatus 报告 value 是否为稳定进度状态。
func IsKnownProgressStatus(value ProgressStatus) bool {
	return slices.Contains(progressStatuses, value)
}

// StateStatus 标识 Runtime 生命周期状态。
type StateStatus string

const (
	StateUninitialized      StateStatus = "uninitialized"
	StatePreparingUV        StateStatus = "preparing_uv"
	StateSyncingRepository  StateStatus = "syncing_repository"
	StatePreparingPython    StateStatus = "preparing_python"
	StateSyncingEnvironment StateStatus = "syncing_environment"
	StateReadyToStart       StateStatus = "ready_to_start"
	StateStartingBackend    StateStatus = "starting_backend"
	StateRunning            StateStatus = "running"
	StateRestarting         StateStatus = "restarting"
	StateStoppingBackend    StateStatus = "stopping_backend"
	StateEnvironmentBroken  StateStatus = "environment_broken"
	StateBackendFailed      StateStatus = "backend_failed"
	StateStopped            StateStatus = "stopped"
)

var stateStatuses = []StateStatus{
	StateUninitialized,
	StatePreparingUV,
	StateSyncingRepository,
	StatePreparingPython,
	StateSyncingEnvironment,
	StateReadyToStart,
	StateStartingBackend,
	StateRunning,
	StateRestarting,
	StateStoppingBackend,
	StateEnvironmentBroken,
	StateBackendFailed,
	StateStopped,
}

// AllStateStatuses 按文档顺序返回全部稳定生命周期状态的防御性副本。
func AllStateStatuses() []StateStatus {
	return append([]StateStatus(nil), stateStatuses...)
}

// IsKnownStateStatus 报告 value 是否为稳定生命周期状态。
func IsKnownStateStatus(value StateStatus) bool {
	return slices.Contains(stateStatuses, value)
}

// Capability 是 hello 事件公告的稳定能力标识。
type Capability string

// 协议 v1 的能力标识全集，逐项含义见 doc/架构设计.md「能力标识全集」。
const (
	CapabilityStdinCancel Capability = "stdin.cancel"
	CapabilityStateV1     Capability = "state.v1"
	CapabilityLogStream   Capability = "log.stream"
	// CapabilityStdinShutdown 与 CapabilityStdinStatus 是 backend supervise 早已
	// 接受、却一直没有公告的两条 stdin 控制命令。README 与架构文档都规定
	// 「实际可用命令以 hello.capabilities 为准」，不公告等于让守规矩的调用方
	// 永远不发 shutdown，优雅关闭退化成 Job 强杀。
	CapabilityStdinShutdown Capability = "stdin.shutdown"
	CapabilityStdinStatus   Capability = "stdin.status"
)

var knownCapabilities = []Capability{
	CapabilityStdinCancel,
	CapabilityStateV1,
	CapabilityLogStream,
	CapabilityStdinShutdown,
	CapabilityStdinStatus,
}

// AllCapabilities 按文档顺序返回全部稳定能力标识的防御性副本。
func AllCapabilities() []Capability {
	return append([]Capability(nil), knownCapabilities...)
}

// IsKnownCapability 报告 value 是否为稳定能力标识。
func IsKnownCapability(value Capability) bool {
	return slices.Contains(knownCapabilities, value)
}
