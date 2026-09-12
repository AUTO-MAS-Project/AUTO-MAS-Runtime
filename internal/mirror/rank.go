package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidRankRequest 表示 RankPlan 的输入 Plan、结果集合或 pinned key 不自洽。
var ErrInvalidRankRequest = errors.New("mirror rank request is invalid")

// Seal 返回 Plan 内容的十六进制摘要，供 ProbeCache 作缓存键。
func (p Plan) Seal() string {
	return hexSeal(p.seal)
}

// newPlan 用给定顺序构造带正确 seal 的在线 Plan；调用方负责保证 sources 满足 validPlan。
func newPlan(kind Kind, sources []Source) Plan {
	plan := Plan{
		kind:    kind,
		sources: append([]Source(nil), sources...),
		offline: false,
	}
	plan.seal = planSeal(plan.kind, plan.sources, plan.offline)
	return plan
}

func hexSeal(seal [sha256.Size]byte) string {
	return hex.EncodeToString(seal[:])
}

// rankGroup 决定排序时的大类：钉住的源、探测成功的源、失败或缺失的源。
type rankGroup uint8

const (
	rankGroupPinned rankGroup = iota
	rankGroupOK
	rankGroupFailed
)

type rankedSource struct {
	source   Source
	position int
	group    rankGroup
	result   ProbeResult
}

// RankPlan 按 C16 第 3 条重排：pinned（显式首选 key，可空）钉最前；其余按 BytesPerSecond 降序、
// TTFB 升序；OK=false 的源排最后但保留原相对顺序；results 缺失的源视为 OK=false。
// 返回的 Plan 与输入同 Kind、同源集合；results 里出现 plan 之外的源或重复源都是错误。
func RankPlan(plan Plan, results []ProbeResult, pinned string) (Plan, error) {
	if !validPlan(plan) || plan.offline {
		return Plan{}, fmt.Errorf("%w: plan", ErrInvalidRankRequest)
	}
	byKey := make(map[string]ProbeResult, len(results))
	for _, result := range results {
		key := result.Source.key
		if _, exists := byKey[key]; exists {
			return Plan{}, fmt.Errorf("%w: duplicate result for %s", ErrInvalidRankRequest, key)
		}
		if !slices.Contains(plan.sources, result.Source) {
			return Plan{}, fmt.Errorf("%w: result source is not in plan", ErrInvalidRankRequest)
		}
		byKey[key] = result
	}
	if pinned != "" {
		if !validSourceKey(pinned) || !slices.ContainsFunc(plan.sources, func(source Source) bool {
			return source.key == pinned
		}) {
			return Plan{}, fmt.Errorf("%w: pinned source is not in plan", ErrInvalidRankRequest)
		}
	}

	ranked := make([]rankedSource, 0, len(plan.sources))
	for position, source := range plan.sources {
		entry := rankedSource{source: source, position: position, group: rankGroupFailed}
		if result, ok := byKey[source.key]; ok && result.OK {
			entry.group = rankGroupOK
			entry.result = result
		}
		if pinned != "" && source.key == pinned {
			entry.group = rankGroupPinned
		}
		ranked = append(ranked, entry)
	}
	slices.SortStableFunc(ranked, compareRanked)

	sources := make([]Source, 0, len(ranked))
	for _, entry := range ranked {
		sources = append(sources, entry.source)
	}
	return newPlan(plan.kind, sources), nil
}

// compareRanked 只在同为探测成功的源之间比较测量值；其余分组保持稳定排序的原相对顺序。
func compareRanked(a, b rankedSource) int {
	if a.group != b.group {
		return int(a.group) - int(b.group)
	}
	if a.group != rankGroupOK {
		return 0
	}
	if a.result.BytesPerSecond != b.result.BytesPerSecond {
		if a.result.BytesPerSecond > b.result.BytesPerSecond {
			return -1
		}
		return 1
	}
	if a.result.TTFB != b.result.TTFB {
		if a.result.TTFB < b.result.TTFB {
			return -1
		}
		return 1
	}
	return 0
}
