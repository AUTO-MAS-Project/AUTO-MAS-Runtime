package relay

import (
	"bytes"
	"net/http"
	"testing"
)

func TestServer_ConcurrentRequestsShareFetch(t *testing.T) {
	content := testContent(120_000, 31)
	mirror := newFakeMirror(t, false)
	mirror.put(wheelPath, content)
	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	mirror.behave(wheelPath, mirrorBehavior{gate: gate, entered: entered})
	fixture := startFixture(t, fixtureOptions{
		cfg: packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, mirror),
	}, mirror)

	const requesters = 3
	results := make([]<-chan fetchResult, 0, requesters)
	for i := 0; i < requesters; i++ {
		results = append(results, fixture.getAsync(relayPath(wheelPath)))
	}
	awaitSignal(t, entered, "first upstream request")
	close(gate)
	for i, pending := range results {
		result := awaitResult(t, pending, "shared response")
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("requester %d result = %+v, want 200", i, result)
		}
		if !bytes.Equal(result.body, content) {
			t.Fatalf("requester %d body mismatch", i)
		}
	}
	// 完成后的重复请求也直接从暂存交付，不再打上游。
	response, body := fixture.get(t, relayPath(wheelPath))
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("follow-up status = %d, want 200 with same content", response.StatusCode)
	}
	if got := mirror.count(wheelPath); got != 1 {
		t.Fatalf("upstream requests = %d, want exactly 1 shared fetch", got)
	}
	summary := fixture.server.Summary()
	if summary.Files != 1 || summary.Bytes != int64(len(content)) {
		t.Errorf("summary = %+v, want one delivered file counted once", summary)
	}
}

func TestServer_MaxFilesBackpressure(t *testing.T) {
	first := testContent(80_000, 41)
	second := testContent(80_000, 42)
	const firstPath = "packages/aa/first-1.0-py3-none-any.whl"
	const secondPath = "packages/bb/second-1.0-py3-none-any.whl"
	mirror := newFakeMirror(t, false)
	mirror.put(firstPath, first)
	mirror.put(secondPath, second)
	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	mirror.behave(firstPath, mirrorBehavior{gate: gate, entered: entered})
	slotWaiting := make(chan struct{}, 8)
	cfg := packagesConfig([]Item{
		itemFor(firstPath[len("packages/"):], first),
		itemFor(secondPath[len("packages/"):], second),
	}, mirror)
	cfg.MaxFiles = 1
	fixture := startFixture(t, fixtureOptions{
		cfg:       cfg,
		internals: internals{slotWaiting: slotWaiting},
	}, mirror)

	firstResult := fixture.getAsync(relayPath(firstPath))
	awaitSignal(t, entered, "first file holding the only slot")
	secondResult := fixture.getAsync(relayPath(secondPath))
	awaitSignal(t, slotWaiting, "second file waiting for a slot")
	if got := mirror.count(secondPath); got != 0 {
		t.Fatalf("second file reached upstream while slot was held: %d requests", got)
	}
	close(gate)
	for name, pending := range map[string]<-chan fetchResult{"first": firstResult, "second": secondResult} {
		result := awaitResult(t, pending, name+" response")
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("%s result = %+v, want 200", name, result)
		}
	}
	if got := mirror.count(secondPath); got != 1 {
		t.Fatalf("second file upstream requests = %d, want 1 after slot released", got)
	}
	if got := fixture.server.Summary().Files; got != 2 {
		t.Errorf("delivered files = %d, want 2", got)
	}
}
