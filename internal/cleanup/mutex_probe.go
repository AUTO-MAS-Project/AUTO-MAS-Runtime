package cleanup

import (
	"context"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// peerMutexProbe 从对端视角报告 mutation Mutex 占用。
//
// cleanup 已持有 mutation 租约，任何 peer 都不可能持有同一 Mutex；
// 因此对端探测恒为 free，更新事务的活动性只由 PID 探针区分
// stale/inconsistent，这与 state.InspectTransaction 的语义一致。
type peerMutexProbe struct{}

func (peerMutexProbe) Probe(
	ctx context.Context,
	kind state.MutexKind,
) (state.MutexProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return state.MutexProbeResult{}, err
	}
	if !kind.Valid() {
		return state.MutexProbeResult{}, fmt.Errorf("invalid mutex kind %q", kind)
	}
	return state.MutexProbeResult{Held: false, Recovered: false}, nil
}

var _ state.MutexProbe = peerMutexProbe{}
