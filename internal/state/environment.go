package state

import (
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Revision 标识一个已验证的产品版本和 Git Commit。
type Revision struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// BrokenEnvironment 保存当前 active revision 无法启动主环境的原因。
type BrokenEnvironment struct {
	TargetVersion string         `json:"targetVersion"`
	Branch        string         `json:"branch"`
	Commit        string         `json:"commit"`
	PythonVersion string         `json:"pythonVersion"`
	UVVersion     string         `json:"uvVersion"`
	Reason        BrokenReason   `json:"reason"`
	Stage         protocol.Stage `json:"stage"`
	ExitCode      int            `json:"exitCode"`
	LogPath       string         `json:"logPath"`
}

// EnvironmentState 是跨 Runtime 进程持久化的稳定环境真相。
type EnvironmentState struct {
	SchemaVersion  int                  `json:"schemaVersion"`
	Status         protocol.StateStatus `json:"status"`
	UpdatedAt      time.Time            `json:"updatedAt"`
	LastSuccessful Revision             `json:"lastSuccessful"`
	Broken         *BrokenEnvironment   `json:"broken"`
}

// NewReadyEnvironment 构造依赖已同步成功的稳定状态。
func (s *Store) NewReadyEnvironment(
	version string,
	commit string,
) (EnvironmentState, error) {
	if s == nil {
		return EnvironmentState{}, validationError("store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updatedAt, err := s.sampleTimeLocked("updatedAt")
	if err != nil {
		return EnvironmentState{}, err
	}
	value := EnvironmentState{
		SchemaVersion: SchemaVersion,
		Status:        protocol.StateReadyToStart,
		UpdatedAt:     updatedAt,
		LastSuccessful: Revision{
			Version: version,
			Commit:  commit,
		},
		Broken: nil,
	}
	if err := validateEnvironment(s.layout, value); err != nil {
		return EnvironmentState{}, err
	}
	return value, nil
}

// NewBrokenEnvironment 构造保留最近成功 revision 的失效状态。
func (s *Store) NewBrokenEnvironment(
	lastSuccessful Revision,
	broken BrokenEnvironment,
) (EnvironmentState, error) {
	if s == nil {
		return EnvironmentState{}, validationError("store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updatedAt, err := s.sampleTimeLocked("updatedAt")
	if err != nil {
		return EnvironmentState{}, err
	}
	copyOfBroken := broken
	value := EnvironmentState{
		SchemaVersion:  SchemaVersion,
		Status:         protocol.StateEnvironmentBroken,
		UpdatedAt:      updatedAt,
		LastSuccessful: lastSuccessful,
		Broken:         &copyOfBroken,
	}
	if err := validateEnvironment(s.layout, value); err != nil {
		return EnvironmentState{}, err
	}
	return value, nil
}

// ValidateEnvironment 对内存输入和磁盘解码值执行同一稳定状态规则。
func (s *Store) ValidateEnvironment(value EnvironmentState) error {
	if s == nil || s.layout == nil {
		return validationError("store")
	}
	return validateEnvironment(s.layout, value)
}

func validateEnvironment(
	layout *config.Layout,
	value EnvironmentState,
) error {
	if value.SchemaVersion != SchemaVersion {
		return validationError("schemaVersion")
	}
	if value.Status != protocol.StateReadyToStart &&
		value.Status != protocol.StateEnvironmentBroken {
		return validationError("status")
	}
	if err := validateTimestamp("updatedAt", value.UpdatedAt); err != nil {
		return err
	}
	if err := validateRevision(value.LastSuccessful); err != nil {
		return err
	}
	switch value.Status {
	case protocol.StateReadyToStart:
		if value.LastSuccessful.Version == "" ||
			value.LastSuccessful.Commit == "" {
			return validationError("lastSuccessful")
		}
		if value.Broken != nil {
			return validationError("broken")
		}
		return nil
	case protocol.StateEnvironmentBroken:
		if value.Broken == nil {
			return validationError("broken")
		}
		return validateBrokenEnvironment(layout, *value.Broken)
	default:
		return validationError("status")
	}
}

func validateRevision(value Revision) error {
	if (value.Version == "") != (value.Commit == "") {
		return validationError("lastSuccessful")
	}
	if value.Version == "" {
		return nil
	}
	if err := validateProductVersion(value.Version); err != nil {
		return validationError("lastSuccessful")
	}
	if err := validateCommit(value.Commit); err != nil {
		return validationError("lastSuccessful")
	}
	return nil
}

func validateBrokenEnvironment(
	layout *config.Layout,
	value BrokenEnvironment,
) error {
	if value.TargetVersion == "" {
		return validationError("targetVersion")
	}
	if err := validateProductVersion(value.TargetVersion); err != nil {
		return validationError("targetVersion")
	}
	if value.Branch != "release/"+value.TargetVersion ||
		containsControl(value.Branch) {
		return validationError("branch")
	}
	if err := validateCommit(value.Commit); err != nil {
		return validationError("commit")
	}
	if value.PythonVersion != "" {
		if err := validateToolVersion(value.PythonVersion); err != nil {
			return validationError("pythonVersion")
		}
	}
	if value.UVVersion != "" {
		if err := validateToolVersion(value.UVVersion); err != nil {
			return validationError("uvVersion")
		}
	}
	if !value.Reason.Valid() {
		return validationError("reason")
	}
	if !protocol.IsKnownStage(value.Stage) {
		return validationError("stage")
	}
	if err := validateRuntimeLogPath(layout, value.LogPath); err != nil {
		return err
	}

	switch value.Reason {
	case ReasonRepositoryChanged:
		if value.Stage != protocol.StageWorkspaceSwap {
			return validationError("stage")
		}
		if value.ExitCode != 0 {
			return validationError("exitCode")
		}
	case ReasonOperationFailed:
		if !operationFailedStage(value.Stage) {
			return validationError("stage")
		}
		if value.ExitCode <= 0 {
			return validationError("exitCode")
		}
		if operationFailedToolsRequired(value.Stage) &&
			(value.PythonVersion == "" || value.UVVersion == "") {
			return validationError("toolVersion")
		}
	default:
		return validationError("reason")
	}
	return nil
}

func operationFailedStage(stage protocol.Stage) bool {
	switch stage {
	case protocol.StageWorkspaceCleanup, protocol.StagePythonCheck,
		protocol.StagePythonInstall, protocol.StageDependenciesCheck,
		protocol.StageDependenciesSync, protocol.StageDependenciesRebuild:
		return true
	default:
		return false
	}
}

func operationFailedToolsRequired(stage protocol.Stage) bool {
	switch stage {
	case protocol.StagePythonInstall, protocol.StageDependenciesCheck,
		protocol.StageDependenciesSync, protocol.StageDependenciesRebuild:
		return true
	default:
		return false
	}
}
