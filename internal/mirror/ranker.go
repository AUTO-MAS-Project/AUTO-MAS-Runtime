package mirror

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInvalidRankerOption 表示 NewRanker 的参数或 option 非法。
var ErrInvalidRankerOption = errors.New("mirror ranker option is invalid")

// sourceProber 是 Ranker 消费的最小探测能力；生产实现是 *Prober。
type sourceProber interface {
	Probe(ctx context.Context, plan Plan, target ProbeTarget, report func(ProbeResult)) ([]ProbeResult, error)
}

// ProbeReportFunc 在某个源探测完成时同步回调：completed 为该 Kind 已完成的源数，total 为源总数。
// 返回错误会让本次排序失败并上抛（协议输出已断时没有必要继续）。
type ProbeReportFunc func(kind Kind, result ProbeResult, completed, total int) error

// RankedFunc 在某个 Kind 完成一轮探测并排好序后同步回调一次（缓存命中不回调），供发出 network.probe 的收口事件。
// 返回错误会让本次排序失败并上抛。
type RankedFunc func(kind Kind, ranked Plan, results []ProbeResult) error

// Ranker 把 Prober、RankPlan 与 ProbeCache 组合成一个 PlanFunc（增补 2 C16）。
//
// 没有探针目标的 Kind 按目录顺序返回、不联网；探针失败（全部源失败、探测器出错）只会退回目录顺序，
// 永远不让操作失败。同一 Ranker 可被多个 goroutine 并发使用。
type Ranker struct {
	catalog *Catalog
	prober  sourceProber
	cache   *ProbeCache
	report  ProbeReportFunc
	ranked  RankedFunc
	logger  func(message string)

	mu      sync.Mutex // 保护 targets、results 与 inflight
	targets map[Kind]ProbeTarget
	results map[Kind][]ProbeResult
	// inflight 让同一 Kind 的并发首次调用只探测一次，其余调用等待结果。
	inflight map[Kind]chan struct{}
}

// RankerOption 配置构造后只读的 Ranker 依赖。
type RankerOption func(*Ranker) error

// WithRankerProber 注入探测器；不注入时使用默认 HTTP 客户端与 3 秒预算的 Prober。
func WithRankerProber(prober *Prober) RankerOption {
	return func(ranker *Ranker) error {
		if prober == nil {
			return fmt.Errorf("%w: prober", ErrInvalidRankerOption)
		}
		ranker.prober = prober
		return nil
	}
}

// ProbeFunc 是可注入的探测实现，形态与 (*Prober).Probe 相同；供其它包的测试替换真实探测。
type ProbeFunc func(ctx context.Context, plan Plan, target ProbeTarget, report func(ProbeResult)) ([]ProbeResult, error)

// Probe 让 ProbeFunc 满足 Ranker 的探测依赖。
func (f ProbeFunc) Probe(ctx context.Context, plan Plan, target ProbeTarget, report func(ProbeResult)) ([]ProbeResult, error) {
	return f(ctx, plan, target, report)
}

// WithRankerProbeFunc 注入探测函数（测试替身）；nil 视为参数错误。
func WithRankerProbeFunc(probe ProbeFunc) RankerOption {
	return func(ranker *Ranker) error {
		if probe == nil {
			return fmt.Errorf("%w: probe func", ErrInvalidRankerOption)
		}
		ranker.prober = probe
		return nil
	}
}

// withRankerSourceProber 注入任意探测实现，包内测试替身用。
func withRankerSourceProber(prober sourceProber) RankerOption {
	return func(ranker *Ranker) error {
		if prober == nil {
			return fmt.Errorf("%w: prober", ErrInvalidRankerOption)
		}
		ranker.prober = prober
		return nil
	}
}

// WithRankerCache 注入实测顺序缓存；不注入时使用 30 分钟 TTL 的进程内缓存。
func WithRankerCache(cache *ProbeCache) RankerOption {
	return func(ranker *Ranker) error {
		if cache == nil {
			return fmt.Errorf("%w: cache", ErrInvalidRankerOption)
		}
		ranker.cache = cache
		return nil
	}
}

// WithRankerReport 注入探测进度回调（network.probe 事件的来源）。
func WithRankerReport(report ProbeReportFunc) RankerOption {
	return func(ranker *Ranker) error {
		if report == nil {
			return fmt.Errorf("%w: report", ErrInvalidRankerOption)
		}
		ranker.report = report
		return nil
	}
}

// WithRankerRanked 注入每轮探测排序完成后的回调（network.probe 收口事件的来源）。
func WithRankerRanked(ranked RankedFunc) RankerOption {
	return func(ranker *Ranker) error {
		if ranked == nil {
			return fmt.Errorf("%w: ranked callback", ErrInvalidRankerOption)
		}
		ranker.ranked = ranked
		return nil
	}
}

// WithRankerLogger 注入探测失败时的诊断日志出口（写操作日志，不是 stdout）。
func WithRankerLogger(logger func(message string)) RankerOption {
	return func(ranker *Ranker) error {
		if logger == nil {
			return fmt.Errorf("%w: logger", ErrInvalidRankerOption)
		}
		ranker.logger = logger
		return nil
	}
}

// NewRanker 创建按实测排序的 PlanFunc 提供者。
func NewRanker(catalog *Catalog, options ...RankerOption) (*Ranker, error) {
	if err := validateCatalog(catalog); err != nil {
		return nil, fmt.Errorf("%w: catalog", ErrInvalidRankerOption)
	}
	ranker := &Ranker{
		catalog:  catalog,
		cache:    NewProbeCache(time.Now, DefaultProbeCacheTTL),
		targets:  make(map[Kind]ProbeTarget, len(AllKinds())),
		results:  make(map[Kind][]ProbeResult, len(AllKinds())),
		inflight: make(map[Kind]chan struct{}, len(AllKinds())),
	}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil option", ErrInvalidRankerOption)
		}
		if err := option(ranker); err != nil {
			return nil, err
		}
	}
	if ranker.prober == nil {
		prober, err := NewProber()
		if err != nil {
			return nil, err
		}
		ranker.prober = prober
	}
	return ranker, nil
}

// SetTarget 登记某个 Kind 的探针目标；uv 与 git 的目标在启动时已知，python 与 package-index 由各自阶段在探测前登记。
func (r *Ranker) SetTarget(kind Kind, target ProbeTarget) {
	if r == nil || !kind.Valid() || target.Kind != kind {
		return
	}
	r.mu.Lock()
	r.targets[kind] = target
	r.mu.Unlock()
}

// Results 返回某个 Kind 最近一次探测的结果（按最终尝试顺序）；未探测过返回 nil。
func (r *Ranker) Results(kind Kind) []ProbeResult {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	results, ok := r.results[kind]
	if !ok {
		return nil
	}
	return append([]ProbeResult(nil), results...)
}

// PlanFunc 返回可注入各消费方的尝试顺序构造函数。
func (r *Ranker) PlanFunc() PlanFunc {
	return r.Plan
}

// Warm 依次为给定 Kind 取一次顺序，用于在首个网络 stage 之前把测速做完并发出事件。
func (r *Ranker) Warm(ctx context.Context, policy Policy, kinds ...Kind) error {
	for _, kind := range kinds {
		if _, err := r.Plan(ctx, policy, kind); err != nil {
			return err
		}
	}
	return nil
}

// Plan 是 PlanFunc 的实现：目录顺序 → 缓存命中直接返回 → 否则有目标就探测并排序。
func (r *Ranker) Plan(ctx context.Context, policy Policy, kind Kind) (Plan, error) {
	if r == nil {
		return Plan{}, fmt.Errorf("%w: ranker is nil", ErrInvalidRankerOption)
	}
	if ctx == nil {
		return Plan{}, fmt.Errorf("%w: nil context", ErrInvalidRankerOption)
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	base, err := BuildPlan(r.catalog, policy, kind)
	if err != nil {
		return Plan{}, err
	}
	if base.Offline() {
		return base, nil
	}
	seal := base.Seal()
	if ranked, _, ok := r.cache.Lookup(kind, seal); ok {
		return ranked, nil
	}
	r.mu.Lock()
	target, hasTarget := r.targets[kind]
	if !hasTarget {
		r.mu.Unlock()
		return base, nil
	}
	if waiter, running := r.inflight[kind]; running {
		r.mu.Unlock()
		select {
		case <-waiter:
		case <-ctx.Done():
			return Plan{}, ctx.Err()
		}
		if ranked, _, ok := r.cache.Lookup(kind, seal); ok {
			return ranked, nil
		}
		return base, nil
	}
	done := make(chan struct{})
	r.inflight[kind] = done
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, kind)
		r.mu.Unlock()
		close(done)
	}()

	pinned, _ := policy.Preferred(kind)
	ranked, results, err := r.probeAndRank(ctx, base, target, pinned)
	if err != nil {
		return Plan{}, err
	}
	r.mu.Lock()
	r.results[kind] = results
	r.mu.Unlock()
	r.cache.Store(kind, seal, ranked, results)
	if r.ranked != nil {
		if err := r.ranked(kind, ranked, append([]ProbeResult(nil), results...)); err != nil {
			return Plan{}, err
		}
	}
	return ranked, nil
}

// probeAndRank 执行一次探测并排序；探测失败退回 base，只有取消与报告回调错误会返回 error。
func (r *Ranker) probeAndRank(ctx context.Context, base Plan, target ProbeTarget, pinned string) (Plan, []ProbeResult, error) {
	total := len(base.sources)
	completed := 0
	var reportErr error
	results, err := r.prober.Probe(ctx, base, target, func(result ProbeResult) {
		if r.report == nil || reportErr != nil {
			return
		}
		completed++
		if callbackErr := r.report(base.kind, result, completed, total); callbackErr != nil {
			reportErr = callbackErr
		}
	})
	if reportErr != nil {
		return Plan{}, nil, reportErr
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Plan{}, nil, err
		}
		r.log(fmt.Sprintf("network probe for %s failed, keeping catalog order: %v", base.kind, err))
		return base, nil, nil
	}
	ranked, rankErr := RankPlan(base, results, pinned)
	if rankErr != nil {
		r.log(fmt.Sprintf("network ranking for %s failed, keeping catalog order: %v", base.kind, rankErr))
		return base, results, nil
	}
	return ranked, orderResults(ranked, results), nil
}

// orderResults 把探测结果按最终尝试顺序重排，供 result.details.networkProbe 直接输出。
func orderResults(ranked Plan, results []ProbeResult) []ProbeResult {
	byKey := make(map[string]ProbeResult, len(results))
	for _, result := range results {
		byKey[result.Source.Key()] = result
	}
	ordered := make([]ProbeResult, 0, len(ranked.sources))
	for _, source := range ranked.sources {
		if result, ok := byKey[source.Key()]; ok {
			ordered = append(ordered, result)
		}
	}
	return ordered
}

func (r *Ranker) log(message string) {
	if r.logger != nil {
		r.logger(message)
	}
}
