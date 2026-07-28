package protocol

import "slices"

// Stage identifies a stable Runtime operation stage.
type Stage string

const (
	StageRuntimeHandshake    Stage = "runtime.handshake"
	StageDoctor              Stage = "doctor"
	StageBootstrap           Stage = "bootstrap"
	StageRepair              Stage = "repair"
	StageCleanup             Stage = "cleanup"
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

// AllStages returns every stable stage in document order.
func AllStages() []Stage {
	return append([]Stage(nil), stages...)
}

// IsKnownStage reports whether value is a stable Runtime stage.
func IsKnownStage(value Stage) bool {
	return slices.Contains(stages, value)
}

// ProgressStatus identifies the state of progress for a stage.
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

// AllProgressStatuses returns every stable progress status in document order.
func AllProgressStatuses() []ProgressStatus {
	return append([]ProgressStatus(nil), progressStatuses...)
}

// IsKnownProgressStatus reports whether value is a stable progress status.
func IsKnownProgressStatus(value ProgressStatus) bool {
	return slices.Contains(progressStatuses, value)
}

// StateStatus identifies a Runtime lifecycle state.
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

// AllStateStatuses returns every stable lifecycle state in document order.
func AllStateStatuses() []StateStatus {
	return append([]StateStatus(nil), stateStatuses...)
}

// IsKnownStateStatus reports whether value is a stable lifecycle state.
func IsKnownStateStatus(value StateStatus) bool {
	return slices.Contains(stateStatuses, value)
}
