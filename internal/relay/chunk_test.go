package relay

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const bigWheel = "packages/ab/cd/4567/big-2.0-cp312-cp312-win_amd64.whl"

func chunkedConfig(items []Item, mirrors ...*fakeMirror) Config {
	cfg := packagesConfig(items, mirrors...)
	cfg.ChunkThreshold = 64 << 10
	cfg.ChunkSize = 16 << 10
	return cfg
}

func rangeStart(t *testing.T, spec string) int64 {
	t.Helper()
	spec = strings.TrimPrefix(spec, "bytes=")
	start, err := strconv.ParseInt(strings.SplitN(spec, "-", 2)[0], 10, 64)
	if err != nil {
		t.Fatalf("parse range %q: %v", spec, err)
	}
	return start
}

func TestFetch_ChunksAcrossRangeSources(t *testing.T) {
	content := testContent(256<<10, 7)
	item := itemFor(bigWheel[len("packages/"):], content)
	mirrors := make([]*fakeMirror, 0, 4)
	gates := make([]chan struct{}, 0, 4)
	entered := make(chan struct{}, 64)
	for i := 0; i < 4; i++ {
		mirror := newFakeMirror(t, true)
		mirror.put(bigWheel, content)
		gate := make(chan struct{})
		mirror.behave(bigWheel, mirrorBehavior{gate: gate, entered: entered})
		mirrors = append(mirrors, mirror)
		gates = append(gates, gate)
	}
	fixture := startFixture(t, fixtureOptions{cfg: chunkedConfig([]Item{item}, mirrors...)}, mirrors...)

	results := fixture.getAsync(relayPath(bigWheel))
	// barrier：四个 worker 各自拿到第一片并到达上游后才放行，保证每个源都至少服务一片。
	for i := 0; i < 4; i++ {
		awaitSignal(t, entered, "worker entry at upstream")
	}
	for _, gate := range gates {
		close(gate)
	}
	result := awaitResult(t, results, "chunked response")
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("result = %+v, want 200", result)
	}
	if !bytes.Equal(result.body, content) {
		t.Fatalf("assembled body mismatch (%d bytes, want %d)", len(result.body), len(content))
	}
	summary := fixture.server.Summary()
	var total int64
	for i, mirror := range mirrors {
		key := string(rune('a' + i))
		served := 0
		for _, request := range mirror.recorded() {
			if request.path != bigWheel {
				continue
			}
			if request.rangeSpec == "" {
				t.Errorf("mirror %s got a request without Range: %+v", key, request)
			}
			served++
		}
		if served == 0 {
			t.Errorf("mirror %s served no chunks", key)
		}
		if summary.BySource[key] <= 0 {
			t.Errorf("bySource[%s] = %d, want > 0", key, summary.BySource[key])
		}
		total += summary.BySource[key]
	}
	if total != int64(len(content)) || summary.Files != 1 || summary.Bytes != int64(len(content)) {
		t.Errorf("summary = %+v, want %d bytes across sources", summary, len(content))
	}
	final := fixture.progress.snapshot()
	if len(final) == 0 || final[len(final)-1].Received != int64(len(content)) || final[len(final)-1].Source == "" {
		t.Errorf("final progress = %+v, want received=%d with a source", final, len(content))
	}
}

func TestFetch_ChunkFailureRequeues(t *testing.T) {
	content := testContent(256<<10, 8)
	item := itemFor(bigWheel[len("packages/"):], content)
	flaky := newFakeMirror(t, true)
	flaky.put(bigWheel, content)
	flaky.behave(bigWheel, mirrorBehavior{failRangesFrom: 64 << 10})
	steady := newFakeMirror(t, true)
	steady.put(bigWheel, content)
	fixture := startFixture(t, fixtureOptions{cfg: chunkedConfig([]Item{item}, flaky, steady)}, flaky, steady)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("assembled body mismatch (%d bytes, want %d)", len(body), len(content))
	}
	failedRanges := 0
	for _, request := range flaky.recorded() {
		if request.path == bigWheel && request.rangeSpec != "" && rangeStart(t, request.rangeSpec) >= 64<<10 {
			failedRanges++
		}
	}
	if failedRanges != 1 {
		t.Errorf("flaky mirror failing-range requests = %d, want exactly 1 (worker exits after first failure)", failedRanges)
	}
	if got := fixture.logs.outcomes("a"); len(got) != 1 || got[0] != OutcomeHTTPStatus {
		t.Errorf("outcomes for a = %v, want [http_status]", got)
	}
	summary := fixture.server.Summary()
	if summary.BySource["a"]+summary.BySource["b"] != int64(len(content)) || summary.BySource["b"] < 192<<10 {
		t.Errorf("bySource = %v, want b to serve every chunk from 64 KiB on", summary.BySource)
	}
	if len(summary.Failures) != 0 {
		t.Errorf("failures = %v, want none", summary.Failures)
	}
}

func TestFetch_ChunkSourcesExhaustedFallsBackToSingleStream(t *testing.T) {
	content := testContent(256<<10, 9)
	item := itemFor(bigWheel[len("packages/"):], content)
	rangeA := newFakeMirror(t, true)
	rangeA.put(bigWheel, content)
	rangeA.behave(bigWheel, mirrorBehavior{failRangesFrom: 32 << 10})
	rangeB := newFakeMirror(t, true)
	rangeB.put(bigWheel, content)
	rangeB.behave(bigWheel, mirrorBehavior{failRangesFrom: 32 << 10})
	plain := newFakeMirror(t, false)
	plain.put(bigWheel, content)
	fixture := startFixture(t, fixtureOptions{cfg: chunkedConfig([]Item{item}, rangeA, rangeB, plain)}, rangeA, rangeB, plain)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("status = %d body %d bytes, want 200 with %d bytes", response.StatusCode, len(body), len(content))
	}
	plainRequests := plain.recorded()
	if len(plainRequests) != 1 || plainRequests[0].rangeSpec != "" || plainRequests[0].method != http.MethodGet {
		t.Fatalf("plain mirror requests = %+v, want one full GET", plainRequests)
	}
	summary := fixture.server.Summary()
	if summary.BySource["c"] != int64(len(content)) || summary.Files != 1 {
		t.Errorf("summary = %+v, want the whole file credited to c", summary)
	}
}

func TestFetch_NoRangeSupportUsesSingleStream(t *testing.T) {
	content := testContent(256<<10, 10)
	item := itemFor(bigWheel[len("packages/"):], content)
	ranged := newFakeMirror(t, true)
	ranged.put(bigWheel, content)
	plain := newFakeMirror(t, false)
	plain.put(bigWheel, content)
	fixture := startFixture(t, fixtureOptions{cfg: chunkedConfig([]Item{item}, ranged, plain)}, ranged, plain)

	response, body := fixture.get(t, relayPath(bigWheel))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("status = %d body %d bytes, want 200 with %d bytes", response.StatusCode, len(body), len(content))
	}
	requests := ranged.recorded()
	if len(requests) != 1 || requests[0].rangeSpec != "" {
		t.Errorf("ranged mirror requests = %+v, want exactly one request without Range", requests)
	}
	if got := plain.countAll(); got != 0 {
		t.Errorf("plain mirror requests = %d, want 0", got)
	}
}
