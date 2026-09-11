package mirror

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRankerProber 按 key 返回预设吞吐，记录被探测的次数与目标；不碰网络。
type fakeRankerProber struct {
	mu      sync.Mutex // 保护 calls 与 targets
	calls   int
	targets []ProbeTarget
	speeds  map[string]int64
	err     error
}

func (f *fakeRankerProber) Probe(ctx context.Context, plan Plan, target ProbeTarget, report func(ProbeResult)) ([]ProbeResult, error) {
	f.mu.Lock()
	f.calls++
	f.targets = append(f.targets, target)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := make([]ProbeResult, 0, len(plan.sources))
	for _, source := range plan.sources {
		speed, ok := f.speeds[source.Key()]
		result := ProbeResult{Source: source, OK: ok, BytesPerSecond: speed, TTFB: 10 * time.Millisecond, AcceptRanges: ok}
		if !ok {
			result.Err = errors.New("probe failed")
		}
		if report != nil {
			report(result)
		}
		results = append(results, result)
	}
	return results, nil
}

func (f *fakeRankerProber) probeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func rankerTestPolicy(t *testing.T, spec PolicySpec) Policy {
	t.Helper()
	if spec.Preferred == nil {
		spec.Preferred = map[Kind]string{}
	}
	policy, err := NewPolicy(spec)
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	return policy
}

func rankerTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	return catalog
}

func planKeys(plan Plan) string {
	keys := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		keys = append(keys, source.Key())
	}
	return strings.Join(keys, ",")
}

// TestRanker_PlanFuncRanksAndCaches 锁定 Ranker：首次调用探测并按吞吐排序（官方源可赢），
// 报告回调按源逐个触发，第二次调用命中缓存不再探测。
func TestRanker_PlanFuncRanksAndCaches(t *testing.T) {
	t.Parallel()

	prober := &fakeRankerProber{speeds: map[string]int64{
		"agentsmirror": 200_000, "gh-proxy": 3_000_000, "cdn-gh-proxy": 900_000, "edgeone-gh-proxy": 50_000, "github": 8_000_000,
	}}
	var mu sync.Mutex
	reports := make([]string, 0, 5)
	ranker, err := NewRanker(rankerTestCatalog(t),
		withRankerSourceProber(prober),
		WithRankerReport(func(kind Kind, result ProbeResult, completed, total int) error {
			mu.Lock()
			defer mu.Unlock()
			if kind != KindUV || total != 5 || completed != len(reports)+1 {
				t.Errorf("report kind=%s completed=%d total=%d", kind, completed, total)
			}
			reports = append(reports, result.Source.Key())
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	target := ProbeTarget{Kind: KindUV, Path: "0.12.3/uv-x86_64-pc-windows-msvc.zip", WindowBytes: DefaultProbeWindowBytes}
	ranker.SetTarget(KindUV, target)
	policy := rankerTestPolicy(t, PolicySpec{})

	plan, err := ranker.PlanFunc()(context.Background(), policy, KindUV)
	if err != nil {
		t.Fatalf("PlanFunc() error = %v", err)
	}
	if got := planKeys(plan); got != "github,gh-proxy,cdn-gh-proxy,agentsmirror,edgeone-gh-proxy" {
		t.Fatalf("ranked order = %s", got)
	}
	if len(reports) != 5 || prober.probeCalls() != 1 || prober.targets[0] != target {
		t.Fatalf("reports = %v, probes = %d, targets = %+v", reports, prober.probeCalls(), prober.targets)
	}
	results := ranker.Results(KindUV)
	if len(results) != 5 || results[0].Source.Key() != "github" || results[0].BytesPerSecond != 8_000_000 {
		t.Fatalf("Results() = %+v, want github first", results)
	}

	again, err := ranker.PlanFunc()(context.Background(), policy, KindUV)
	if err != nil {
		t.Fatalf("second PlanFunc() error = %v", err)
	}
	if again.Seal() != plan.Seal() || prober.probeCalls() != 1 {
		t.Fatalf("second call re-probed: probes = %d", prober.probeCalls())
	}
}

// TestRanker_WithoutTargetFallsBackToCatalogOrder 锁定没有探针目标的 Kind 按目录顺序返回、不探测。
func TestRanker_WithoutTargetFallsBackToCatalogOrder(t *testing.T) {
	t.Parallel()

	prober := &fakeRankerProber{speeds: map[string]int64{"github": 1}}
	ranker, err := NewRanker(rankerTestCatalog(t), withRankerSourceProber(prober))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	policy := rankerTestPolicy(t, PolicySpec{})
	plan, err := ranker.PlanFunc()(context.Background(), policy, KindGit)
	if err != nil {
		t.Fatalf("PlanFunc() error = %v", err)
	}
	want, err := BuildPlan(rankerTestCatalog(t), policy, KindGit)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if plan.Seal() != want.Seal() || prober.probeCalls() != 0 {
		t.Fatalf("plan without target = %s (probes %d), want catalog order", planKeys(plan), prober.probeCalls())
	}
	if results := ranker.Results(KindGit); results != nil {
		t.Fatalf("Results() = %+v, want nil without probing", results)
	}
}

// TestRanker_ProbeFailuresNeverFail 锁定探针全部失败或探针器出错时仍返回目录顺序；报告回调错误则上抛。
func TestRanker_ProbeFailuresNeverFail(t *testing.T) {
	t.Parallel()

	policy := rankerTestPolicy(t, PolicySpec{})
	target := ProbeTarget{Kind: KindPython, Path: "20260807/x.tar.gz", WindowBytes: DefaultProbeWindowBytes}
	want, err := BuildPlan(rankerTestCatalog(t), policy, KindPython)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}

	allFailed := &fakeRankerProber{speeds: map[string]int64{}}
	ranker, err := NewRanker(rankerTestCatalog(t), withRankerSourceProber(allFailed))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	ranker.SetTarget(KindPython, target)
	plan, err := ranker.PlanFunc()(context.Background(), policy, KindPython)
	if err != nil {
		t.Fatalf("PlanFunc() error = %v", err)
	}
	if plan.Seal() != want.Seal() {
		t.Fatalf("plan after total probe failure = %s, want catalog order %s", planKeys(plan), planKeys(want))
	}
	for _, result := range ranker.Results(KindPython) {
		if result.OK {
			t.Fatalf("result %+v OK, want failed", result)
		}
	}

	broken := &fakeRankerProber{err: errors.New("prober exploded")}
	ranker, err = NewRanker(rankerTestCatalog(t), withRankerSourceProber(broken))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	ranker.SetTarget(KindPython, target)
	plan, err = ranker.PlanFunc()(context.Background(), policy, KindPython)
	if err != nil || plan.Seal() != want.Seal() {
		t.Fatalf("plan after prober error = %s, err = %v, want catalog order", planKeys(plan), err)
	}

	reportErr := errors.New("stdout closed")
	reporting, err := NewRanker(rankerTestCatalog(t),
		withRankerSourceProber(&fakeRankerProber{speeds: map[string]int64{"github": 1}}),
		WithRankerReport(func(Kind, ProbeResult, int, int) error { return reportErr }),
	)
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	reporting.SetTarget(KindPython, target)
	if _, err := reporting.PlanFunc()(context.Background(), policy, KindPython); !errors.Is(err, reportErr) {
		t.Fatalf("PlanFunc() error = %v, want report error", err)
	}
}

// TestRanker_PinnedPreferredStaysFirst 锁定显式首选不被实测结果挪动，离线策略不探测。
func TestRanker_PinnedPreferredStaysFirst(t *testing.T) {
	t.Parallel()

	prober := &fakeRankerProber{speeds: map[string]int64{"cnb": 10, "github": 1_000_000}}
	ranker, err := NewRanker(rankerTestCatalog(t), withRankerSourceProber(prober))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	ranker.SetTarget(KindGit, ProbeTarget{Kind: KindGit, Path: "info/refs?service=git-upload-pack"})
	pinned := rankerTestPolicy(t, PolicySpec{Preferred: map[Kind]string{KindGit: "cnb"}})
	plan, err := ranker.PlanFunc()(context.Background(), pinned, KindGit)
	if err != nil {
		t.Fatalf("PlanFunc() error = %v", err)
	}
	if got := planKeys(plan); got != "cnb,github" {
		t.Fatalf("pinned order = %s, want cnb,github", got)
	}

	offline, err := NewPolicy(PolicySpec{Offline: true})
	if err != nil {
		t.Fatalf("NewPolicy(offline) error = %v", err)
	}
	offlinePlan, err := ranker.PlanFunc()(context.Background(), offline, KindGit)
	if err != nil || !offlinePlan.Offline() || prober.probeCalls() != 1 {
		t.Fatalf("offline plan = %+v, err = %v, probes = %d; want offline without probing", offlinePlan, err, prober.probeCalls())
	}
}

// TestRanker_WarmProbesRequestedKinds 锁定 Warm 只探测有目标的 Kind，并对同一 Kind 只探一次。
func TestRanker_WarmProbesRequestedKinds(t *testing.T) {
	t.Parallel()

	prober := &fakeRankerProber{speeds: map[string]int64{"github": 1}}
	ranker, err := NewRanker(rankerTestCatalog(t), withRankerSourceProber(prober))
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	ranker.SetTarget(KindUV, ProbeTarget{Kind: KindUV, Path: "0.12.3/uv.zip", WindowBytes: 1024})
	ranker.SetTarget(KindGit, ProbeTarget{Kind: KindGit, Path: "info/refs?service=git-upload-pack"})
	policy := rankerTestPolicy(t, PolicySpec{})
	if err := ranker.Warm(context.Background(), policy, KindUV, KindGit, KindPython); err != nil {
		t.Fatalf("Warm() error = %v", err)
	}
	if prober.probeCalls() != 2 {
		t.Fatalf("probes after Warm = %d, want 2 (python has no target)", prober.probeCalls())
	}
	if err := ranker.Warm(context.Background(), policy, KindUV); err != nil || prober.probeCalls() != 2 {
		t.Fatalf("second Warm re-probed: err = %v, probes = %d", err, prober.probeCalls())
	}
	if _, err := NewRanker(nil); err == nil {
		t.Fatal("NewRanker(nil) error = nil, want error")
	}
	if _, err := NewRanker(rankerTestCatalog(t), WithRankerProber(nil)); err == nil {
		t.Fatal("WithRankerProber(nil) error = nil, want error")
	}
}
