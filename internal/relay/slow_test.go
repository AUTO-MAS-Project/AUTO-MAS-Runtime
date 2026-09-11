package relay

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestFetch_SlowChunkSourceIsDemoted 锁定 T14.8：分片取回时一个源在片内停住（限速源的极端形态），
// 快源完成两片后把它判慢并取消其片，它已写下的字节保留、剩余范围由快源接手；慢源不算失败，
// 但被挪到该路由顺序末位，后续文件不再优先选它。
func TestFetch_SlowChunkSourceIsDemoted(t *testing.T) {
	content := testContent(256<<10, 9)
	item := itemFor(bigWheel[len("packages/"):], content)
	slow := newFakeMirror(t, true)
	slow.put(bigWheel, content)
	slow.behave(bigWheel, mirrorBehavior{hang: true, hangAt: 1024})
	fast := newFakeMirror(t, true)
	fast.put(bigWheel, content)
	// 快源先被闸住，直到中继确实收下了慢源那 1 KiB：这样判慢一定发生在慢源已有进度之后，
	// 断言的字节归属才是确定的。
	fastGate := make(chan struct{})
	fast.behave(bigWheel, mirrorBehavior{gate: fastGate})
	releaseFast := gateOnSourceProgress("a", 1024, fastGate)
	fixture := startFixture(t, fixtureOptions{
		cfg:  chunkedConfig([]Item{item}, slow, fast),
		deps: Deps{Progress: releaseFast},
	}, slow, fast)
	// 虚拟时钟每读一次前进 1 秒：判慢的最小在飞时长（1.5 s）在两次读取之后即满足；真实时间不参与。
	fixture.clock.setStep(time.Second)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("assembled body mismatch (%d bytes, want %d)", len(body), len(content))
	}
	if got := slow.count(bigWheel); got != 1 {
		t.Errorf("slow mirror requests = %d, want exactly the one chunk it stalled on", got)
	}
	summary := fixture.server.Summary()
	if summary.Files != 1 || summary.Bytes != int64(len(content)) || len(summary.Failures) != 0 {
		t.Fatalf("summary = %+v, want one complete file without failures", summary)
	}
	if summary.BySource["a"] != 1024 {
		t.Errorf("slow source credited %d bytes, want the 1024 it delivered before demotion", summary.BySource["a"])
	}
	if summary.BySource["b"] != int64(len(content))-1024 {
		t.Errorf("fast source credited %d bytes, want the remainder", summary.BySource["b"])
	}
	// 判慢发生在快源完成两片之后（不是收尾）：慢源片的剩余范围（它停住的位置 + 1 KiB 起）回队后位于栈顶，
	// 快源随后几个请求里就有它（被取消的 worker 要过几个调度点才回队，允许到第 7 个请求，
	// 但绝不是收尾时的最后一个——收尾判慢才会那样）。慢源先拿到哪一片由调度决定，所以从它的请求里读起点。
	slowRequests := slow.recorded()
	if len(slowRequests) != 1 {
		t.Fatalf("slow requests = %+v, want exactly one", slowRequests)
	}
	remainderStart := rangeStart(t, slowRequests[0].rangeSpec) + 1024
	recorded := fast.recorded()
	position := -1
	for index, request := range recorded {
		if request.rangeSpec != "" && rangeStart(t, request.rangeSpec) == remainderStart {
			position = index
			break
		}
	}
	if position < 0 || position > 6 || position == len(recorded)-1 {
		t.Errorf("demoted remainder (start %d) was fast request #%d of %d, want it picked up right after demotion", remainderStart, position+1, len(recorded))
	}
	engine, ok := fixture.server.fetcher.(*engine)
	if !ok {
		t.Fatal("server fetcher is not the engine")
	}
	order := engine.snapshotUpstreams(RoutePackages)
	if len(order) != 2 || order[0].Key != "b" || order[1].Key != "a" {
		t.Fatalf("upstream order after demotion = %+v, want the slow source moved last", order)
	}
	if got := fixture.logs.outcomes("a"); len(got) != 0 {
		t.Errorf("slow source recorded failure outcomes %v, want none (demotion is not a failure)", got)
	}
}

// TestFetch_TailLaggardIsDemoted 锁定收尾判慢：队列已空、快源至少完成过一片、慢源的最后一片进度未过半时，
// 快源接手其剩余范围而不是等它慢慢取完。
func TestFetch_TailLaggardIsDemoted(t *testing.T) {
	content := testContent(32<<10, 5)
	item := itemFor(bigWheel[len("packages/"):], content)
	slow := newFakeMirror(t, true)
	slow.put(bigWheel, content)
	slow.behave(bigWheel, mirrorBehavior{hang: true, hangAt: 1024})
	fast := newFakeMirror(t, true)
	fast.put(bigWheel, content)
	fastGate := make(chan struct{})
	fast.behave(bigWheel, mirrorBehavior{gate: fastGate})
	cfg := chunkedConfig([]Item{item}, slow, fast)
	// 两片：慢源拿第一片就停在 1 KiB，快源取完第二片后队列已空——只有收尾判慢能救这一片。
	cfg.ChunkThreshold = 32 << 10
	cfg.ChunkSize = 16 << 10
	fixture := startFixture(t, fixtureOptions{cfg: cfg, deps: Deps{Progress: gateOnSourceProgress("a", 1024, fastGate)}}, slow, fast)
	fixture.clock.setStep(time.Second)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("status = %d, body %d bytes; want 200 with %d bytes", response.StatusCode, len(body), len(content))
	}
	summary := fixture.server.Summary()
	if summary.BySource["a"] != 1024 || summary.BySource["b"] != int64(len(content))-1024 {
		t.Fatalf("bySource = %v, want slow 1024 and fast the rest", summary.BySource)
	}
	if len(summary.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", summary.Failures)
	}
}

// TestChunkPlan_MinimumAgeGatesDemotion 锁定最小在飞时长：片龄不足 1.5 s 时无论落后多少都不判慢
// （首字节延迟、连接建立与调度抖动不算慢），片龄够了才按进度差判定；收尾判慢同样受此约束。
func TestChunkPlan_MinimumAgeGatesDemotion(t *testing.T) {
	clock := newFakeClock()
	plan := newChunkPlan(clock.now)
	piece := chunk{start: 0, end: 16<<10 - 1}
	ctx := plan.begin(context.Background(), "a", piece)
	defer plan.end("a")
	plan.progress("a", 512)
	for i := 0; i < slowSourceCompletedLead; i++ {
		plan.complete()
	}
	if demoted := plan.demoteLaggards("b"); len(demoted) != 0 || ctx.Err() != nil {
		t.Fatalf("demoted %v with age 0, want none", demoted)
	}
	if plan.demoteTailLaggard("b", true) || ctx.Err() != nil {
		t.Fatal("tail demotion fired with age 0, want none")
	}
	clock.advance(slowSourceMinAge)
	if demoted := plan.demoteLaggards("b"); len(demoted) != 1 || demoted[0] != "a" || ctx.Err() == nil {
		t.Fatalf("demoted %v after min age, want [a] with its chunk cancelled", demoted)
	}
	if !plan.isSlow("a") {
		t.Fatal("source a not marked slow")
	}
}

// TestFetch_AllSlowSourcesAreNotDemoted 锁定「都慢不算慢」：没有快源作参照时不降级，仍靠失速超时换源。
func TestFetch_AllSlowSourcesAreNotDemoted(t *testing.T) {
	content := testContent(64<<10, 6)
	item := itemFor(bigWheel[len("packages/"):], content)
	first := newFakeMirror(t, true)
	first.put(bigWheel, content)
	second := newFakeMirror(t, true)
	second.put(bigWheel, content)
	fixture := startFixture(t, fixtureOptions{cfg: chunkedConfig([]Item{item}, first, second)}, first, second)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("status = %d, body %d bytes", response.StatusCode, len(body))
	}
	engine := fixture.server.fetcher.(*engine)
	order := engine.snapshotUpstreams(RoutePackages)
	if order[0].Key != "a" || order[1].Key != "b" {
		t.Fatalf("upstream order changed without any laggard: %+v", order)
	}
}

// gateOnSourceProgress 返回一个进度回调：当 source 累计交付达到 bytes 时关闭 gate（只关一次）。
func gateOnSourceProgress(source string, bytes int64, gate chan struct{}) func(Progress) error {
	var once sync.Once
	return func(progress Progress) error {
		if progress.Source == source && progress.Received >= bytes {
			once.Do(func() { close(gate) })
		}
		return nil
	}
}
