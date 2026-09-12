package mirror

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// cacheTestResults 让吞吐随 plan 位置递增，排序后顺序必然与输入相反，seal 才能区分两者。
func cacheTestResults(t *testing.T, plan Plan) []ProbeResult {
	t.Helper()
	sources := plan.Sources()
	results := make([]ProbeResult, 0, len(sources))
	for index, source := range sources {
		results = append(results, ProbeResult{
			Source:         source,
			OK:             true,
			TTFB:           time.Duration(index+1) * time.Millisecond,
			BytesPerSecond: int64(index+1) * 1_000_000,
			Bytes:          DefaultProbeWindowBytes,
		})
	}
	return results
}

func TestProbeCache_ExpiresAfterTTL(t *testing.T) {
	clock := newFakeProbeClock()
	cache := NewProbeCache(clock.Now, 30*time.Minute)
	plan := mustOnlinePlan(t, KindUV)
	results := cacheTestResults(t, plan)
	ranked, err := RankPlan(plan, results, "")
	if err != nil {
		t.Fatalf("RankPlan() error = %v", err)
	}
	if _, _, ok := cache.Lookup(KindUV, plan.Seal()); ok {
		t.Fatal("Lookup() on empty cache = hit, want miss")
	}
	cache.Store(KindUV, plan.Seal(), ranked, results)

	clock.Advance(30*time.Minute - time.Nanosecond)
	gotPlan, gotResults, ok := cache.Lookup(KindUV, plan.Seal())
	if !ok {
		t.Fatal("Lookup() just before TTL = miss, want hit")
	}
	if gotPlan.Seal() != ranked.Seal() || !reflect.DeepEqual(planSourceKeys(gotPlan), planSourceKeys(ranked)) {
		t.Fatalf("Lookup() plan = %v, want %v", planSourceKeys(gotPlan), planSourceKeys(ranked))
	}
	if !reflect.DeepEqual(gotResults, results) {
		t.Fatalf("Lookup() results = %+v, want %+v", gotResults, results)
	}
	// 返回的切片是副本，调用方改写不能污染缓存。
	gotResults[0].BytesPerSecond = -1
	again, againResults, ok := cache.Lookup(KindUV, plan.Seal())
	if !ok || againResults[0].BytesPerSecond != results[0].BytesPerSecond || again.Seal() != ranked.Seal() {
		t.Fatalf("Lookup() after caller mutation = %+v, want untouched cache", againResults)
	}

	clock.Advance(time.Nanosecond)
	if _, _, ok := cache.Lookup(KindUV, plan.Seal()); ok {
		t.Fatal("Lookup() at TTL = hit, want miss")
	}
	// 过期后重新 Store 会从新的时间点起算。
	cache.Store(KindUV, plan.Seal(), ranked, results)
	clock.Advance(29 * time.Minute)
	if _, _, ok := cache.Lookup(KindUV, plan.Seal()); !ok {
		t.Fatal("Lookup() after re-store = miss, want hit")
	}
}

func TestProbeCache_KeyedBySeal(t *testing.T) {
	clock := newFakeProbeClock()
	cache := NewProbeCache(clock.Now, time.Hour)
	uvPlan := mustOnlinePlan(t, KindUV)
	uvResults := cacheTestResults(t, uvPlan)
	uvRanked, err := RankPlan(uvPlan, uvResults, "")
	if err != nil {
		t.Fatalf("RankPlan(uv) error = %v", err)
	}
	cache.Store(KindUV, uvPlan.Seal(), uvRanked, uvResults)

	if _, _, ok := cache.Lookup(KindUV, uvRanked.Seal()); ok {
		t.Fatal("Lookup() with ranked seal = hit, want miss (key is the input plan seal)")
	}
	if _, _, ok := cache.Lookup(KindPython, uvPlan.Seal()); ok {
		t.Fatal("Lookup() with another kind = hit, want miss")
	}
	if _, _, ok := cache.Lookup(KindUV, ""); ok {
		t.Fatal("Lookup() with empty seal = hit, want miss")
	}

	// 同 Kind 的另一份 plan（例如 --mirror-only 去掉官方源）替换缓存项，而不是并存。
	mirrorOnly, err := NewPolicy(PolicySpec{MirrorOnly: true})
	if err != nil {
		t.Fatalf("NewPolicy(mirror only) error = %v", err)
	}
	narrowPlan, err := BuildPlan(mustDefaultCatalog(t), mirrorOnly, KindUV)
	if err != nil {
		t.Fatalf("BuildPlan(mirror only) error = %v", err)
	}
	narrowResults := cacheTestResults(t, narrowPlan)
	narrowRanked, err := RankPlan(narrowPlan, narrowResults, "")
	if err != nil {
		t.Fatalf("RankPlan(narrow) error = %v", err)
	}
	cache.Store(KindUV, narrowPlan.Seal(), narrowRanked, narrowResults)
	gotPlan, _, ok := cache.Lookup(KindUV, narrowPlan.Seal())
	if !ok || gotPlan.Seal() != narrowRanked.Seal() {
		t.Fatalf("Lookup(narrow) = %v/%t, want narrow ranked plan", planSourceKeys(gotPlan), ok)
	}
	if _, _, ok := cache.Lookup(KindUV, uvPlan.Seal()); ok {
		t.Fatal("Lookup(previous seal) = hit after storing another plan for the same kind, want miss")
	}

	// 非法输入被忽略，不会写入缓存。
	cache.Store(KindGit, uvPlan.Seal(), uvRanked, uvResults)
	if _, _, ok := cache.Lookup(KindGit, uvPlan.Seal()); ok {
		t.Fatal("Store() with kind mismatch was cached")
	}
	cache.Store(KindUV, "", uvRanked, uvResults)
	if _, _, ok := cache.Lookup(KindUV, ""); ok {
		t.Fatal("Store() with empty seal was cached")
	}
	cache.Store(KindPython, uvPlan.Seal(), Plan{}, nil)
	if _, _, ok := cache.Lookup(KindPython, uvPlan.Seal()); ok {
		t.Fatal("Store() with invalid plan was cached")
	}
}

func TestProbeCache_ConcurrentAccess(t *testing.T) {
	clock := newFakeProbeClock()
	cache := NewProbeCache(clock.Now, time.Hour)
	plans := make(map[Kind]Plan, len(AllKinds()))
	results := make(map[Kind][]ProbeResult, len(AllKinds()))
	for _, kind := range AllKinds() {
		plan := mustOnlinePlan(t, kind)
		plans[kind] = plan
		results[kind] = cacheTestResults(t, plan)
	}

	const workers = 8
	const rounds = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for round := 0; round < rounds; round++ {
				kind := AllKinds()[(worker+round)%len(AllKinds())]
				plan := plans[kind]
				ranked, err := RankPlan(plan, results[kind], "")
				if err != nil {
					t.Errorf("RankPlan(%s) error = %v", kind, err)
					return
				}
				cache.Store(kind, plan.Seal(), ranked, results[kind])
				gotPlan, gotResults, ok := cache.Lookup(kind, plan.Seal())
				if !ok {
					t.Errorf("Lookup(%s) = miss right after Store", kind)
					return
				}
				if gotPlan.Kind() != kind || len(gotResults) != len(results[kind]) {
					t.Errorf("Lookup(%s) = kind %s with %d results, want %d", kind, gotPlan.Kind(), len(gotResults), len(results[kind]))
					return
				}
				if round%10 == 0 {
					clock.Advance(time.Millisecond)
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	for _, kind := range AllKinds() {
		if _, _, ok := cache.Lookup(kind, plans[kind].Seal()); !ok {
			t.Fatalf("Lookup(%s) after concurrent workers = miss, want hit", kind)
		}
	}
}

func TestProbeCache_ZeroValueAndDefaults(t *testing.T) {
	var cache ProbeCache
	plan := mustOnlinePlan(t, KindGit)
	if _, _, ok := cache.Lookup(KindGit, plan.Seal()); ok {
		t.Fatal("zero-value Lookup() = hit, want miss")
	}
	cache.Store(KindGit, plan.Seal(), plan, cacheTestResults(t, plan))
	if _, _, ok := cache.Lookup(KindGit, plan.Seal()); !ok {
		t.Fatal("zero-value cache Store/Lookup = miss, want hit with default clock and TTL")
	}
	defaulted := NewProbeCache(nil, 0)
	if defaulted == nil || defaulted.ttl != DefaultProbeCacheTTL || defaulted.clock == nil {
		t.Fatalf("NewProbeCache(nil, 0) = %+v, want default clock and %s TTL", defaulted, DefaultProbeCacheTTL)
	}
	var nilCache *ProbeCache
	if _, _, ok := nilCache.Lookup(KindGit, plan.Seal()); ok {
		t.Fatal("nil cache Lookup() = hit, want miss")
	}
	nilCache.Store(KindGit, plan.Seal(), plan, nil)
}
