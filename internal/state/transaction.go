package state

import (
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// SchemaVersion 是首版状态文件唯一支持的 schema。
const SchemaVersion = 1

// TransactionState 是三类事务文件共享的完整 JSON schema。
type TransactionState struct {
	SchemaVersion int            `json:"schemaVersion"`
	OperationID   string         `json:"operationId"`
	Command       string         `json:"command"`
	PID           uint32         `json:"pid"`
	StartedAt     time.Time      `json:"startedAt"`
	TargetVersion string         `json:"targetVersion"`
	Stage         protocol.Stage `json:"stage"`
}

// TransactionInput 是创建新事务时由应用服务提供的业务身份。
type TransactionInput struct {
	OperationID   string
	Command       string
	PID           uint32
	TargetVersion string
	Stage         protocol.Stage
}

// NewTransaction 采样一次时钟并构造已完整校验的事务状态。
func (s *Store) NewTransaction(
	kind TransactionKind,
	input TransactionInput,
) (TransactionState, error) {
	if s == nil {
		return TransactionState{}, validationError("store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	startedAt, err := s.sampleTimeLocked("startedAt")
	if err != nil {
		return TransactionState{}, err
	}
	value := TransactionState{
		SchemaVersion: SchemaVersion,
		OperationID:   input.OperationID,
		Command:       input.Command,
		PID:           input.PID,
		StartedAt:     startedAt,
		TargetVersion: input.TargetVersion,
		Stage:         input.Stage,
	}
	if err := ValidateTransaction(kind, value); err != nil {
		return TransactionState{}, err
	}
	return value, nil
}

// ValidateTransaction 对内存输入和磁盘解码值执行同一语义规则。
func ValidateTransaction(kind TransactionKind, value TransactionState) error {
	if !kind.Valid() {
		return validationError("kind")
	}
	if value.SchemaVersion != SchemaVersion {
		return validationError("schemaVersion")
	}
	if err := validateOperationID(value.OperationID); err != nil {
		return validationError("operationId")
	}
	if !transactionCommandAllowed(kind, value.Command) {
		return validationError("command")
	}
	if value.PID == 0 {
		return validationError("pid")
	}
	if err := validateTimestamp("startedAt", value.StartedAt); err != nil {
		return err
	}
	if !protocol.IsKnownStage(value.Stage) ||
		!transactionStageAllowed(kind, value.Command, value.Stage) {
		return validationError("stage")
	}
	if value.TargetVersion != "" {
		if err := validateProductVersion(value.TargetVersion); err != nil {
			return validationError("targetVersion")
		}
	}
	if transactionTargetRequired(kind, value.Command) && value.TargetVersion == "" {
		return validationError("targetVersion")
	}
	return nil
}

func transactionCommandAllowed(kind TransactionKind, command string) bool {
	switch kind {
	case TransactionBackend:
		return command == "backend supervise"
	case TransactionMutation:
		switch command {
		case "bootstrap", "workspace sync", "environment ensure", "environment repair",
			"dependencies sync", "dependencies rebuild", "repair", "cleanup":
			return true
		default:
			return false
		}
	case TransactionUpdate:
		return command == "bootstrap" || command == "workspace sync"
	default:
		return false
	}
}

func transactionStageAllowed(
	kind TransactionKind,
	command string,
	stage protocol.Stage,
) bool {
	switch kind {
	case TransactionBackend:
		switch stage {
		case protocol.StageBackendSpawn, protocol.StageBackendHealth,
			protocol.StageBackendRun, protocol.StageBackendRestart,
			protocol.StageBackendShutdown, protocol.StageBackendCleanup:
			return true
		default:
			return false
		}
	case TransactionUpdate:
		return workspaceMutationStage(stage) &&
			(stage != protocol.StageWorkspaceCheck)
	case TransactionMutation:
		return mutationCommandStageAllowed(command, stage)
	default:
		return false
	}
}

func mutationCommandStageAllowed(command string, stage protocol.Stage) bool {
	switch command {
	case "bootstrap":
		return stage == protocol.StageBootstrap ||
			uvStage(stage) || workspaceStage(stage) ||
			pythonStage(stage) || dependencyStage(stage)
	case "workspace sync":
		return workspaceStage(stage)
	case "environment ensure":
		return uvStage(stage)
	case "environment repair":
		return uvStage(stage) || pythonStage(stage)
	case "dependencies sync":
		return stage == protocol.StageDependenciesCheck ||
			stage == protocol.StageDependenciesSync
	case "dependencies rebuild":
		return dependencyStage(stage)
	case "repair":
		return stage == protocol.StageRepair ||
			uvStage(stage) || pythonStage(stage) || dependencyStage(stage)
	case "cleanup":
		return stage == protocol.StageCleanup ||
			stage == protocol.StageWorkspaceCleanup
	default:
		return false
	}
}

func uvStage(stage protocol.Stage) bool {
	switch stage {
	case protocol.StageUVCheck, protocol.StageUVDownload, protocol.StageUVVerify:
		return true
	default:
		return false
	}
}

func workspaceStage(stage protocol.Stage) bool {
	return stage == protocol.StageWorkspaceCheck || workspaceMutationStage(stage)
}

func workspaceMutationStage(stage protocol.Stage) bool {
	switch stage {
	case protocol.StageWorkspaceClone, protocol.StageWorkspaceVerify,
		protocol.StageWorkspaceSwap, protocol.StageWorkspaceCleanup:
		return true
	default:
		return false
	}
}

func pythonStage(stage protocol.Stage) bool {
	return stage == protocol.StagePythonCheck ||
		stage == protocol.StagePythonInstall
}

func dependencyStage(stage protocol.Stage) bool {
	switch stage {
	case protocol.StageDependenciesCheck, protocol.StageDependenciesSync,
		protocol.StageDependenciesRebuild:
		return true
	default:
		return false
	}
}

func transactionTargetRequired(kind TransactionKind, command string) bool {
	return kind == TransactionUpdate ||
		kind == TransactionMutation &&
			(command == "bootstrap" || command == "workspace sync")
}
