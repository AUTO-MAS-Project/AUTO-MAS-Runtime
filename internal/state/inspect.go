package state

import "context"

// PIDProbe 查询一个已知 PID 是否仍存活。
type PIDProbe interface {
	Alive(ctx context.Context, pid uint32) (bool, error)
}

// MutexProbeResult 保存零等待探测与 abandoned 恢复事实。
type MutexProbeResult struct {
	Held      bool
	Recovered bool
}

// MutexProbe 按 state 绑定的 MutexKind 查询真实占用。
type MutexProbe interface {
	Probe(ctx context.Context, kind MutexKind) (MutexProbeResult, error)
}

// Inspection 是事务文件与 Mutex/PID 事实的失败关闭分类。
type Inspection struct {
	Activity       Activity
	MutexHeld      bool
	MutexRecovered bool
	PIDChecked     bool
	PIDAlive       bool
	ProbeError     error
}

// InspectTransaction 先查询权威 Mutex，只在 free 时查询 PID。
func InspectTransaction(
	ctx context.Context,
	kind TransactionKind,
	value TransactionState,
	pidProbe PIDProbe,
	mutexProbe MutexProbe,
) Inspection {
	inspection := Inspection{Activity: ActivityUnknown}
	if ctx == nil {
		inspection.ProbeError = validationError("ctx")
		return inspection
	}
	if !kind.Valid() {
		inspection.ProbeError = validationError("kind")
		return inspection
	}
	if err := ValidateTransaction(kind, value); err != nil {
		inspection.ProbeError = err
		return inspection
	}
	if pidProbe == nil {
		inspection.ProbeError = validationError("pidProbe")
		return inspection
	}
	if mutexProbe == nil {
		inspection.ProbeError = validationError("mutexProbe")
		return inspection
	}
	if err := ctx.Err(); err != nil {
		inspection.ProbeError = err
		return inspection
	}
	mutexKind, err := transactionMutexKind(kind)
	if err != nil {
		inspection.ProbeError = err
		return inspection
	}
	mutexResult, err := mutexProbe.Probe(ctx, mutexKind)
	inspection.MutexHeld = mutexResult.Held
	inspection.MutexRecovered = mutexResult.Recovered
	if err != nil {
		inspection.ProbeError = err
		return inspection
	}
	if mutexResult.Held {
		inspection.Activity = ActivityActive
		return inspection
	}
	if err := ctx.Err(); err != nil {
		inspection.ProbeError = err
		return inspection
	}

	inspection.PIDChecked = true
	alive, err := pidProbe.Alive(ctx, value.PID)
	inspection.PIDAlive = alive && err == nil
	if err != nil {
		inspection.ProbeError = err
		return inspection
	}
	if err := ctx.Err(); err != nil {
		inspection.ProbeError = err
		inspection.PIDAlive = false
		return inspection
	}
	if alive {
		inspection.Activity = ActivityInconsistent
		return inspection
	}
	inspection.Activity = ActivityStale
	return inspection
}

// CanAutoClean 只报告 stale 候选；调用方仍须持 Lease 重读并条件删除。
func (i Inspection) CanAutoClean() bool {
	return i.Activity == ActivityStale
}

func transactionMutexKind(kind TransactionKind) (MutexKind, error) {
	switch kind {
	case TransactionBackend:
		return MutexBackend, nil
	case TransactionMutation, TransactionUpdate:
		return MutexMutation, nil
	default:
		return "", validationError("kind")
	}
}
