package filesystem

import (
	"fmt"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// MaxStateFileBytes 是受管状态文件默认允许的最大字节数。
const MaxStateFileBytes int64 = 256 * 1024

// StateFileKind 标识受管状态文件类别。
type StateFileKind string

const (
	StateBackend     StateFileKind = "backend"
	StateMutation    StateFileKind = "mutation"
	StateUpdate      StateFileKind = "update"
	StateEnvironment StateFileKind = "environment"
)

func (k StateFileKind) String() string { return string(k) }

func (k StateFileKind) Valid() bool {
	switch k {
	case StateBackend, StateMutation, StateUpdate, StateEnvironment:
		return true
	default:
		return false
	}
}

// StateWritePhase 标识原子状态写入失败发生的阶段。
type StateWritePhase string

const (
	StateWritePhaseRecover  StateWritePhase = "recover"
	StateWritePhaseCreate   StateWritePhase = "create"
	StateWritePhaseWrite    StateWritePhase = "write"
	StateWritePhaseSync     StateWritePhase = "sync"
	StateWritePhaseRename   StateWritePhase = "rename"
	StateWritePhaseFinalize StateWritePhase = "finalize"
	StateWritePhaseClose    StateWritePhase = "close"
)

func (p StateWritePhase) String() string { return string(p) }

func (p StateWritePhase) Valid() bool {
	switch p {
	case StateWritePhaseRecover,
		StateWritePhaseCreate,
		StateWritePhaseWrite,
		StateWritePhaseSync,
		StateWritePhaseRename,
		StateWritePhaseFinalize,
		StateWritePhaseClose:
		return true
	default:
		return false
	}
}

type stateFileOwner struct {
	marker byte
}

// StateFiles 表示受管状态目录能力。
type StateFiles struct {
	mu sync.RWMutex // 保护 closed、closeErr、pins、probePassed 与操作/关闭的句柄生命周期。

	layout              *config.Layout
	api                 pathAPI
	waitGate            WaitFunc
	fillNonce           func([]byte) error
	afterUnlinkVerified func(string)
	owner               *stateFileOwner
	pins                [2]pinnedObject
	probePassed         map[StateFileKind]bool
	closed              bool
	closeErr            error
}

// StateFileSnapshot 绑定读取内容与其物理文件身份。
type StateFileSnapshot struct {
	owner        *stateFileOwner
	kind         StateFileKind
	volumeSerial uint64
	fileID       [16]byte
	size         int64
	digest       [32]byte
	bytes        []byte
}

// Kind 返回 snapshot 对应的封闭状态文件类别。
func (s StateFileSnapshot) Kind() StateFileKind {
	return s.kind
}

// Bytes 返回 snapshot payload 的防御性副本。
func (s StateFileSnapshot) Bytes() []byte {
	return append([]byte(nil), s.bytes...)
}

// WriteAtomicResult 报告原子写入的副作用与恢复事实。
type WriteAtomicResult struct {
	MutationApplied  bool
	RecoveryRequired bool
}

// StateRemoveResult 报告条件删除的副作用与恢复事实。
type StateRemoveResult struct {
	MutationApplied  bool
	RecoveryRequired bool
}

// StateWriteError 保存写入阶段、主因和独立清理错误。
type StateWriteError struct {
	Phase            StateWritePhase
	MutationApplied  bool
	RecoveryRequired bool
	Cause            error
	CleanupError     error
}

func (e *StateWriteError) Error() string {
	if e == nil {
		return "state write failed"
	}
	if e.Cause == nil {
		return fmt.Sprintf("state write %s failed", e.Phase)
	}
	return fmt.Sprintf("state write %s: %v", e.Phase, e.Cause)
}

func (e *StateWriteError) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.Cause != nil {
		causes = append(causes, e.Cause)
	}
	if e.CleanupError != nil {
		causes = append(causes, e.CleanupError)
	}
	return causes
}

// StateRemoveError 分离条件删除的主因和独立清理错误。
type StateRemoveError struct {
	Cause        error
	CleanupError error
}

func (e *StateRemoveError) Error() string {
	if e == nil {
		return "state remove failed"
	}
	switch {
	case e.Cause != nil && e.CleanupError != nil:
		return "state remove failed; cleanup also failed"
	case e.Cause != nil:
		return "state remove failed"
	case e.CleanupError != nil:
		return "state remove cleanup failed"
	default:
		return "state remove failed"
	}
}

func (e *StateRemoveError) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.Cause != nil {
		causes = append(causes, e.Cause)
	}
	if e.CleanupError != nil {
		causes = append(causes, e.CleanupError)
	}
	return causes
}
