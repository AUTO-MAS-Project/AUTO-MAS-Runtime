package state

// TransactionKind 标识三个互不替代的事务文件。
type TransactionKind string

const (
	// TransactionBackend 标识 backend supervision 事务。
	TransactionBackend TransactionKind = "backend"
	// TransactionMutation 标识顶层受管变更事务。
	TransactionMutation TransactionKind = "mutation"
	// TransactionUpdate 标识 mutation 内的 Git 更新事务。
	TransactionUpdate TransactionKind = "update"
)

// String 返回稳定字面量。
func (k TransactionKind) String() string { return string(k) }

// Valid 报告 kind 是否属于冻结集合。
func (k TransactionKind) Valid() bool {
	switch k {
	case TransactionBackend, TransactionMutation, TransactionUpdate:
		return true
	default:
		return false
	}
}

// BrokenReason 标识环境失效的稳定原因。
type BrokenReason string

const (
	// ReasonRepositoryChanged 表示 active repo 已替换但主环境尚未重新证明可用。
	ReasonRepositoryChanged BrokenReason = "repository_changed"
	// ReasonOperationFailed 表示改变主环境后的具体叶子操作失败。
	ReasonOperationFailed BrokenReason = "operation_failed"
)

// String 返回稳定字面量。
func (r BrokenReason) String() string { return string(r) }

// Valid 报告 reason 是否属于冻结集合。
func (r BrokenReason) Valid() bool {
	switch r {
	case ReasonRepositoryChanged, ReasonOperationFailed:
		return true
	default:
		return false
	}
}

// WritePhase 标识持久化写失败发生的稳定阶段。
type WritePhase string

const (
	// WritePhaseEncode 表示稳定 JSON 编码阶段。
	WritePhaseEncode WritePhase = "encode"
	// WritePhaseRecover 表示 guard 取得、preflight 或既有 intent 恢复阶段。
	WritePhaseRecover WritePhase = "recover"
	// WritePhaseCreate 表示临时状态文件创建阶段。
	WritePhaseCreate WritePhase = "create"
	// WritePhaseWrite 表示完整 payload 写入阶段。
	WritePhaseWrite WritePhase = "write"
	// WritePhaseSync 表示临时文件刷盘阶段。
	WritePhaseSync WritePhase = "sync"
	// WritePhaseRename 表示原子发布阶段。
	WritePhaseRename WritePhase = "rename"
	// WritePhaseFinalize 表示 rollback 或事务 leaf POSIX unlink 阶段。
	WritePhaseFinalize WritePhase = "finalize"
	// WritePhaseClose 表示已打开 handle 的收口阶段。
	WritePhaseClose WritePhase = "close"
	// WritePhaseRemove 表示事务条件删除阶段。
	WritePhaseRemove WritePhase = "remove"
)

// String 返回稳定字面量。
func (p WritePhase) String() string { return string(p) }

// Valid 报告 phase 是否属于冻结集合。
func (p WritePhase) Valid() bool {
	switch p {
	case WritePhaseEncode, WritePhaseRecover, WritePhaseCreate, WritePhaseWrite,
		WritePhaseSync, WritePhaseRename, WritePhaseFinalize, WritePhaseClose,
		WritePhaseRemove:
		return true
	default:
		return false
	}
}

// MutexKind 是 state 消费的两类互斥身份。
type MutexKind string

const (
	// MutexBackend 标识 backend 命名 Mutex。
	MutexBackend MutexKind = "backend"
	// MutexMutation 标识 mutation 与 update 共用的命名 Mutex。
	MutexMutation MutexKind = "mutation"
)

// String 返回稳定字面量。
func (k MutexKind) String() string { return string(k) }

// Valid 报告 kind 是否属于冻结集合。
func (k MutexKind) Valid() bool {
	switch k {
	case MutexBackend, MutexMutation:
		return true
	default:
		return false
	}
}

// Activity 标识事务状态文件与真实占用的分类结果。
type Activity string

const (
	// ActivityActive 表示权威 Mutex 当前被持有。
	ActivityActive Activity = "active"
	// ActivityStale 表示 Mutex free 且记录的 PID 已退出。
	ActivityStale Activity = "stale"
	// ActivityInconsistent 表示 Mutex free 但相同数字的 PID 仍存活。
	ActivityInconsistent Activity = "inconsistent"
	// ActivityUnknown 表示输入或任一必要探针无法确认。
	ActivityUnknown Activity = "unknown"
)

// String 返回稳定字面量。
func (a Activity) String() string { return string(a) }

// Valid 报告 activity 是否属于冻结集合。
func (a Activity) Valid() bool {
	switch a {
	case ActivityActive, ActivityStale, ActivityInconsistent, ActivityUnknown:
		return true
	default:
		return false
	}
}
