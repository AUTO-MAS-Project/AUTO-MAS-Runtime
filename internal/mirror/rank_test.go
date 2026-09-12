package mirror

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func rankTestPlan(t *testing.T, kind Kind, keys ...string) Plan {
	t.Helper()
	sources := make([]Source, 0, len(keys))
	for index, key := range keys {
		sources = append(sources, probeTestSource(
			t,
			kind,
			key,
			"https://"+key+".example.invalid/download",
			index == len(keys)-1,
		))
	}
	return probeTestPlan(t, kind, sources)
}

func sourceByKey(t *testing.T, plan Plan, key string) Source {
	t.Helper()
	for _, source := range plan.Sources() {
		if source.Key() == key {
			return source
		}
	}
	t.Fatalf("plan %v has no source %q", planSourceKeys(plan), key)
	return Source{}
}

func okResult(t *testing.T, plan Plan, key string, bytesPerSecond int64, ttfb time.Duration) ProbeResult {
	t.Helper()
	return ProbeResult{
		Source:         sourceByKey(t, plan, key),
		OK:             true,
		TTFB:           ttfb,
		BytesPerSecond: bytesPerSecond,
		Bytes:          DefaultProbeWindowBytes,
	}
}

func failedResult(t *testing.T, plan Plan, key string) ProbeResult {
	t.Helper()
	return ProbeResult{
		Source: sourceByKey(t, plan, key),
		Err:    errors.New("probe failed"),
	}
}

func TestRankPlan_PinnedStaysFirst(t *testing.T) {
	plan := rankTestPlan(t, KindUV, "pinned", "fast", "faster", "github")
	results := []ProbeResult{
		okResult(t, plan, "pinned", 1_000, time.Second),
		okResult(t, plan, "fast", 5_000_000, 50*time.Millisecond),
		okResult(t, plan, "faster", 9_000_000, 40*time.Millisecond),
		okResult(t, plan, "github", 7_000_000, 30*time.Millisecond),
	}
	ranked, err := RankPlan(plan, results, "pinned")
	if err != nil {
		t.Fatalf("RankPlan() error = %v", err)
	}
	want := []string{"pinned", "faster", "github", "fast"}
	if got := planSourceKeys(ranked); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked keys = %v, want %v", got, want)
	}
	if ranked.Kind() != KindUV || ranked.Offline() {
		t.Fatalf("ranked plan = kind %q offline %t, want uv online", ranked.Kind(), ranked.Offline())
	}
	// 钉住的源即使探测失败也仍在最前。
	results[0] = failedResult(t, plan, "pinned")
	ranked, err = RankPlan(plan, results, "pinned")
	if err != nil {
		t.Fatalf("RankPlan(pinned failed) error = %v", err)
	}
	if got := planSourceKeys(ranked); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked keys with failed pinned = %v, want %v", got, want)
	}
}

func TestRankPlan_FailedSourcesLast(t *testing.T) {
	plan := rankTestPlan(t, KindPython, "broken-a", "slow", "missing", "broken-b", "fast", "github")
	results := []ProbeResult{
		failedResult(t, plan, "broken-b"),
		okResult(t, plan, "fast", 4_000_000, 80*time.Millisecond),
		failedResult(t, plan, "broken-a"),
		okResult(t, plan, "slow", 40_000, 20*time.Millisecond),
		okResult(t, plan, "github", 4_000_000, 60*time.Millisecond),
	}
	ranked, err := RankPlan(plan, results, "")
	if err != nil {
		t.Fatalf("RankPlan() error = %v", err)
	}
	// 相同吞吐按 TTFB 升序；失败与缺失的源排最后并保持 plan 原有相对顺序。
	want := []string{"github", "fast", "slow", "broken-a", "missing", "broken-b"}
	if got := planSourceKeys(ranked); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked keys = %v, want %v", got, want)
	}
}

func TestRankPlan_OfficialMayWin(t *testing.T) {
	plan := mustOnlinePlan(t, KindPackageIndex)
	if got := planSourceKeys(plan); got[len(got)-1] != "pypi" {
		t.Fatalf("catalog order = %v, want official pypi last before ranking", got)
	}
	results := []ProbeResult{
		okResult(t, plan, "aliyun", 300_000, 934*time.Millisecond),
		okResult(t, plan, "tsinghua", 120_000, 2330*time.Millisecond),
		okResult(t, plan, "ustc", 110_000, 2420*time.Millisecond),
		okResult(t, plan, "pypi", 45_000_000, 92*time.Millisecond),
	}
	ranked, err := RankPlan(plan, results, "")
	if err != nil {
		t.Fatalf("RankPlan() error = %v", err)
	}
	want := []string{"pypi", "aliyun", "tsinghua", "ustc"}
	if got := planSourceKeys(ranked); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked keys = %v, want %v", got, want)
	}
	if !ranked.Sources()[0].Official() {
		t.Fatal("ranked first source is not official, want pypi to win on measurement")
	}
}

func TestRankPlan_PreservesSealInvariant(t *testing.T) {
	plan := mustOnlinePlan(t, KindUV)
	results := []ProbeResult{
		okResult(t, plan, "github", 8_000_000, 30*time.Millisecond),
		okResult(t, plan, "agentsmirror", 200_000, 300*time.Millisecond),
	}
	ranked, err := RankPlan(plan, results, "")
	if err != nil {
		t.Fatalf("RankPlan() error = %v", err)
	}
	if !validPlan(ranked) {
		t.Fatalf("ranked plan %v fails validPlan", planSourceKeys(ranked))
	}
	if ranked.Seal() == plan.Seal() {
		t.Fatal("ranked plan seal equals input seal even though order changed")
	}
	if want := planSeal(ranked.kind, ranked.sources, ranked.offline); ranked.seal != want {
		t.Fatal("ranked plan seal does not match planSeal of its own contents")
	}
	if len(ranked.Seal()) != 64 {
		t.Fatalf("Seal() length = %d, want 64 hex characters", len(ranked.Seal()))
	}
	// 未变化的顺序产出与输入相同的 seal，缓存键才稳定。
	same, err := RankPlan(plan, nil, "")
	if err != nil {
		t.Fatalf("RankPlan(no results) error = %v", err)
	}
	if same.Seal() != plan.Seal() {
		t.Fatalf("Seal() without results = %s, want input seal %s", same.Seal(), plan.Seal())
	}
	if got, want := planSourceKeys(same), planSourceKeys(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys without results = %v, want %v", got, want)
	}
	// 排序结果可以直接再交给 Rotator，证明它满足 Run 的请求不变量。
	if err := validateRotationRequest(
		mustRotator(t),
		t.Context(),
		ranked,
		mustRotationTarget(t),
		func(_ context.Context, _ Attempt) AttemptOutcome { return AttemptOutcome{} },
	); err != nil {
		t.Fatalf("validateRotationRequest(ranked) error = %v", err)
	}
}

func TestRankPlan_RejectsInconsistentInput(t *testing.T) {
	plan := mustOnlinePlan(t, KindUV)
	other := mustOnlinePlan(t, KindPython)
	offlinePolicy, err := NewPolicy(PolicySpec{Offline: true})
	if err != nil {
		t.Fatalf("NewPolicy(offline) error = %v", err)
	}
	offline, err := BuildPlan(mustDefaultCatalog(t), offlinePolicy, KindUV)
	if err != nil {
		t.Fatalf("BuildPlan(offline) error = %v", err)
	}
	foreign, err := NewSource(KindUV, "github", "https://elsewhere.example.invalid/uv", true)
	if err != nil {
		t.Fatalf("NewSource(foreign) error = %v", err)
	}
	tests := []struct {
		name    string
		plan    Plan
		results []ProbeResult
		pinned  string
	}{
		{name: "zero plan", plan: Plan{}},
		{name: "offline plan", plan: offline},
		{name: "pinned not in plan", plan: plan, pinned: "nowhere"},
		{name: "pinned invalid key", plan: plan, pinned: "Bad Key"},
		{
			name:    "result from another kind",
			plan:    plan,
			results: []ProbeResult{okResult(t, other, "github", 1, 0)},
		},
		{
			name:    "result with same key but different source",
			plan:    plan,
			results: []ProbeResult{{Source: foreign, OK: true, BytesPerSecond: 1}},
		},
		{
			name: "duplicate result",
			plan: plan,
			results: []ProbeResult{
				okResult(t, plan, "github", 1, 0),
				okResult(t, plan, "github", 2, 0),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranked, err := RankPlan(test.plan, test.results, test.pinned)
			if !errors.Is(err, ErrInvalidRankRequest) {
				t.Fatalf("RankPlan() error = %v, want ErrInvalidRankRequest", err)
			}
			if validPlan(ranked) {
				t.Fatalf("RankPlan() returned a valid plan %v on error", planSourceKeys(ranked))
			}
		})
	}
}

func TestPlan_SealIsHexOfPlanSeal(t *testing.T) {
	plan := mustOnlinePlan(t, KindGit)
	if got, want := plan.Seal(), hexSeal(planSeal(plan.kind, plan.sources, plan.offline)); got != want {
		t.Fatalf("Seal() = %s, want %s", got, want)
	}
	if (Plan{}).Seal() == plan.Seal() {
		t.Fatal("zero plan Seal() equals a real plan seal")
	}
}
