package mirror

import (
	"sync"
	"time"
)

// DefaultProbeCacheTTL 是 C16 第 1 条规定的进程内测速结果有效期。
const DefaultProbeCacheTTL = 30 * time.Minute

type cachedRanking struct {
	seal     string
	plan     Plan
	results  []ProbeResult
	storedAt time.Time
}

// ProbeCache 是进程内按 Kind 缓存的实测顺序，供长驻进程复用；过期即未命中。
// 零值可用（默认时钟与 TTL），并发安全。
type ProbeCache struct {
	clock func() time.Time
	ttl   time.Duration

	mu      sync.Mutex // 保护 entries 及其中每一项
	entries map[Kind]cachedRanking
}

// NewProbeCache 构造缓存；clock 为 nil 时用 time.Now，ttl 非正时用 DefaultProbeCacheTTL。
func NewProbeCache(clock func() time.Time, ttl time.Duration) *ProbeCache {
	if clock == nil {
		clock = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultProbeCacheTTL
	}
	return &ProbeCache{clock: clock, ttl: ttl}
}

// Lookup 返回 kind 下以 seal（输入 plan 的 Seal()）登记且未过期的实测顺序副本。
// 过期项在此处被移除。
func (c *ProbeCache) Lookup(kind Kind, seal string) (Plan, []ProbeResult, bool) {
	if c == nil || !kind.Valid() || seal == "" {
		return Plan{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[kind]
	if !ok || entry.seal != seal {
		return Plan{}, nil, false
	}
	if c.now().Sub(entry.storedAt) >= c.effectiveTTL() {
		delete(c.entries, kind)
		return Plan{}, nil, false
	}
	return entry.plan, append([]ProbeResult(nil), entry.results...), true
}

// Store 登记 kind 的实测顺序，替换同 Kind 的既有项；输入不自洽时静默忽略——
// 缓存只是加速，写不进去的后果仅是下次重新测速。
func (c *ProbeCache) Store(kind Kind, seal string, plan Plan, results []ProbeResult) {
	if c == nil || !kind.Valid() || seal == "" || !validPlan(plan) || plan.kind != kind {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[Kind]cachedRanking, len(AllKinds()))
	}
	c.entries[kind] = cachedRanking{
		seal:     seal,
		plan:     plan,
		results:  append([]ProbeResult(nil), results...),
		storedAt: c.now(),
	}
}

func (c *ProbeCache) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

func (c *ProbeCache) effectiveTTL() time.Duration {
	if c.ttl <= 0 {
		return DefaultProbeCacheTTL
	}
	return c.ttl
}
