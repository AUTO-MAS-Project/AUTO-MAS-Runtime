package relay

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestProgress_TotalGrowsForUnknownFiles(t *testing.T) {
	first := testContent(30_000, 11)
	second := testContent(70_000, 12)
	registered := testContent(20_000, 13)
	const firstPath = "python/20260807/first.tar.gz"
	const secondPath = "python/20260807/second.tar.gz"
	const wheel = "packages/aa/bb/late-1.0-py3-none-any.whl"
	mirror := newFakeMirror(t, true)
	mirror.put(firstPath, first)
	mirror.put(secondPath, second)
	mirror.put(wheel, registered)
	fixture := startFixture(t, fixtureOptions{cfg: Config{Upstreams: map[Route][]Upstream{
		RoutePackages: {packagesUpstream(mirror, "a")},
		RoutePython:   {pythonUpstream(mirror, "a")},
	}}}, mirror)

	if events := fixture.progress.snapshot(); len(events) != 0 {
		t.Fatalf("progress before any request = %v, want none", events)
	}
	response, _ := fixture.get(t, relayPath(firstPath))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", response.StatusCode)
	}
	events := fixture.progress.snapshot()
	if len(events) == 0 {
		t.Fatal("no progress after first file")
	}
	for i, event := range events {
		if event.Total != int64(len(first)) {
			t.Errorf("event %d total = %d, want %d (HEAD size)", i, event.Total, len(first))
		}
		if event.Route != RoutePython || event.Item != "first.tar.gz" || event.Source != "a" {
			t.Errorf("event %d = %+v, want python/first.tar.gz from a", i, event)
		}
	}
	if last := events[len(events)-1]; last.Received != int64(len(first)) {
		t.Errorf("last received after first = %d, want %d", last.Received, len(first))
	}

	response, _ = fixture.get(t, relayPath(secondPath))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", response.StatusCode)
	}
	events = fixture.progress.snapshot()
	last := events[len(events)-1]
	if last.Total != int64(len(first)+len(second)) || last.Received != last.Total {
		t.Errorf("after second: %+v, want total=received=%d", last, len(first)+len(second))
	}

	before := len(events)
	item := itemFor(wheel[len("packages/"):], registered)
	fixture.server.Register([]Item{item})
	fixture.server.Register([]Item{item})
	if got := len(fixture.progress.snapshot()); got != before {
		t.Errorf("Register produced %d callbacks, want 0", got-before)
	}
	response, _ = fixture.get(t, relayPath(wheel))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("registered status = %d, want 200", response.StatusCode)
	}
	events = fixture.progress.snapshot()
	last = events[len(events)-1]
	wantTotal := int64(len(first) + len(second) + len(registered))
	if last.Total != wantTotal || last.Received != wantTotal {
		t.Errorf("after registered item: %+v, want total=received=%d (idempotent Register)", last, wantTotal)
	}
}

func TestProgress_ThrottledTo200ms(t *testing.T) {
	contents := [][]byte{testContent(300_000, 21), testContent(300_000, 22), testContent(300_000, 23)}
	paths := []string{
		"packages/aa/one-1.0-py3-none-any.whl",
		"packages/bb/two-1.0-py3-none-any.whl",
		"packages/cc/three-1.0-py3-none-any.whl",
	}
	mirror := newFakeMirror(t, false)
	items := make([]Item, 0, len(paths))
	for i, path := range paths {
		mirror.put(path, contents[i])
		items = append(items, itemFor(path[len("packages/"):], contents[i]))
	}
	fixture := startFixture(t, fixtureOptions{cfg: packagesConfig(items, mirror)}, mirror)
	total := int64(3 * 300_000)

	// 时钟静止：首个字节报告一次，其后全部被节流，完成时强制一次。
	fixture.get(t, relayPath(paths[0]))
	events := fixture.progress.snapshot()
	if len(events) != 2 {
		t.Fatalf("callbacks with frozen clock = %d, want 2 (first bytes + forced completion): %+v", len(events), events)
	}
	if events[0].Received <= 0 || events[0].Received >= 300_000 || events[0].BytesPerSecond <= 0 {
		t.Errorf("first event = %+v, want partial bytes with throughput", events[0])
	}
	if events[1].Received != 300_000 || events[1].Total != total || events[1].Item != "one-1.0-py3-none-any.whl" {
		t.Errorf("completion event = %+v, want received=300000 total=%d", events[1], total)
	}

	// 推进 2 s：节流窗口已过，首个字节再次报告；吞吐窗口只剩本次的样本。
	fixture.clock.advance(2 * time.Second)
	fixture.get(t, relayPath(paths[1]))
	events = fixture.progress.snapshot()
	if len(events) != 4 {
		t.Fatalf("callbacks after advancing clock = %d, want 4: %+v", len(events), events)
	}
	if events[2].BytesPerSecond <= 0 || events[2].BytesPerSecond >= 300_000 {
		t.Errorf("throughput after window moved = %d, want only the new sample (0 < n < 300000)", events[2].BytesPerSecond)
	}
	if events[3].Received != 600_000 {
		t.Errorf("second completion received = %d, want 600000", events[3].Received)
	}

	// 时钟未推进：第三个文件的首个字节距上次强制报告不足 200 ms，只剩完成时的强制报告。
	fixture.get(t, relayPath(paths[2]))
	events = fixture.progress.snapshot()
	if len(events) != 5 {
		t.Fatalf("callbacks without advancing clock = %d, want 5: %+v", len(events), events)
	}
	if events[4].Received != total || events[4].Total != total {
		t.Errorf("final event = %+v, want received=total=%d", events[4], total)
	}

	// 推进 200 ms 整：恰好达到节流间隔，首个字节必须报告。
	fixture.clock.advance(progressInterval)
	extra := testContent(300_000, 24)
	const extraPath = "packages/dd/four-1.0-py3-none-any.whl"
	mirror.put(extraPath, extra)
	fixture.server.Register([]Item{itemFor(extraPath[len("packages/"):], extra)})
	fixture.get(t, relayPath(extraPath))
	if got := len(fixture.progress.snapshot()); got != 7 {
		t.Fatalf("callbacks after exactly 200ms = %d, want 7", got)
	}
}

func TestSummary_ListsFailuresCapped(t *testing.T) {
	mirror := newFakeMirror(t, false)
	fixture := startFixture(t, fixtureOptions{cfg: packagesConfig(nil, mirror)}, mirror)

	const requested = maxFailures + 8
	for i := 0; i < requested; i++ {
		path := fmt.Sprintf("/packages/aa/missing-%d.whl", i)
		response, _ := fixture.get(t, path)
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("GET %s status = %d, want 502", path, response.StatusCode)
		}
	}
	summary := fixture.server.Summary()
	if summary.Files != 0 || summary.Bytes != 0 || len(summary.BySource) != 0 {
		t.Errorf("summary counts = %+v, want nothing delivered", summary)
	}
	if len(summary.Failures) != maxFailures {
		t.Fatalf("failures = %d, want capped at %d", len(summary.Failures), maxFailures)
	}
	for i, failure := range summary.Failures {
		if failure.Item != fmt.Sprintf("missing-%d.whl", i) {
			t.Errorf("failure %d item = %q, want the first %d in order", i, failure.Item, maxFailures)
		}
		if len(failure.Attempts) != 1 || failure.Attempts[0] != (AttemptOutcome{Source: "a", Outcome: OutcomeHTTPStatus}) {
			t.Errorf("failure %d attempts = %v, want [a http_status]", i, failure.Attempts)
		}
	}
	// Summary 是快照：修改返回值不影响中继内部状态。
	summary.Failures[0].Attempts[0].Source = "mutated"
	summary.BySource["x"] = 1
	if again := fixture.server.Summary(); again.Failures[0].Attempts[0].Source != "a" || len(again.BySource) != 0 {
		t.Errorf("Summary() shares state with the caller: %+v", again)
	}
}
