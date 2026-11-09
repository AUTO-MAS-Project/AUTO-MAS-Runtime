package relay

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
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

func TestServer_QueuesRequestsBeyondHandlerLimit(t *testing.T) {
	content := testContent(80_000, 47)
	mirror := newFakeMirror(t, false)
	mirror.put(wheelPath, content)
	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	mirror.behave(wheelPath, mirrorBehavior{gate: gate, entered: entered})
	handlerWaiting := make(chan struct{}, 8)
	fixture := startFixture(t, fixtureOptions{
		cfg:       packagesConfig([]Item{itemFor(wheelPath[len("packages/"):], content)}, mirror),
		internals: internals{handlerWaiting: handlerWaiting},
	}, mirror)

	requestCount := cap(fixture.server.handlers) + 4
	results := make([]<-chan fetchResult, 0, requestCount)
	for i := 0; i < requestCount; i++ {
		results = append(results, fixture.getAsync(relayPath(wheelPath)))
	}
	awaitSignal(t, entered, "shared upstream request")
	awaitSignal(t, handlerWaiting, "request queued beyond handler limit")
	if got, want := len(fixture.server.handlers), cap(fixture.server.handlers); got != want {
		t.Fatalf("active handlers at queue = %d, want full capacity %d", got, want)
	}

	fixture.logs.mu.Lock()
	var queuedFields map[string]any
	for i := range fixture.logs.entries {
		entry := fixture.logs.entries[i]
		if entry.message == "relay handler waiting for capacity" {
			queuedFields = entry.fields
			break
		}
	}
	if queuedFields != nil {
		// 计数递增与日志写入不共用锁，先记录的日志可能来自后到的等待者。
		waiting, ok := queuedFields["waitingHandlers"].(int64)
		if queuedFields["activeHandlers"] != cap(fixture.server.handlers) || !ok ||
			waiting < 1 || waiting > int64(requestCount-cap(fixture.server.handlers)) ||
			queuedFields["handlerLimit"] != cap(fixture.server.handlers) || queuedFields["route"] != RoutePackages.String() ||
			queuedFields["item"] != "demo-1.0-py3-none-any.whl" {
			fixture.logs.mu.Unlock()
			t.Fatalf("queued request fields = %#v, want capacity and item diagnostics", queuedFields)
		}
	}
	fixture.logs.mu.Unlock()
	if queuedFields == nil {
		t.Fatal("no relay handler queue diagnostic, want one")
	}

	close(gate)
	for i, pending := range results {
		result := awaitResult(t, pending, "queued response")
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("request %d result = %+v, want 200", i, result)
		}
	}
	if got := mirror.count(wheelPath); got != 1 {
		t.Fatalf("upstream requests = %d, want one shared fetch", got)
	}
}

func TestServer_CanceledQueuedRequestDoesNotBlockClose(t *testing.T) {
	mirror := newFakeMirror(t, true)
	handlerWaiting := make(chan struct{}, 8)
	fetcher := &blockingFetcher{entered: make(chan struct{}, 16)}
	fixture := startFixture(t, fixtureOptions{
		cfg:       Config{Upstreams: loopbackConfig(mirror).Upstreams},
		internals: internals{fetcher: fetcher, handlerWaiting: handlerWaiting},
	}, mirror)

	active := make([]<-chan fetchResult, 0, cap(fixture.server.handlers))
	for i := 0; i < cap(fixture.server.handlers); i++ {
		active = append(active, fixture.getAsync(fmt.Sprintf("/packages/a/b/file-%02d.whl", i)))
	}
	for i := 0; i < cap(fixture.server.handlers); i++ {
		awaitSignal(t, fetcher.entered, "active request holding handler capacity")
	}

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.server.BaseURL()+"/packages/a/b/canceled.whl", nil)
		if err != nil {
			canceled <- err
			return
		}
		response, err := fixture.client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		canceled <- err
	}()
	awaitSignal(t, handlerWaiting, "canceled request to wait for handler capacity")
	cancel()
	select {
	case err := <-canceled:
		if err == nil {
			t.Fatal("canceled queued request error = nil, want context cancellation")
		}
	case <-time.After(testWait):
		t.Fatal("timed out waiting for canceled queued request")
	}

	if err := fixture.server.Close(); err != nil {
		t.Fatalf("Close() error = %v, want canceled queued request to release capacity", err)
	}
	if got := fixture.server.waitingHandlers.Load(); got != 0 {
		t.Fatalf("waiting handlers after Close = %d, want 0", got)
	}
	if got := fixture.server.inflight.Load(); got != 0 {
		t.Fatalf("inflight handlers after Close = %d, want 0", got)
	}
	for i, pending := range active {
		result := awaitResult(t, pending, "active request closed with relay")
		if result.err == nil && result.status == http.StatusServiceUnavailable {
			t.Fatalf("active request %d got 503, want cancellation or 502", i)
		}
	}
}

func TestServer_CloseUnblocksQueuedHandlers(t *testing.T) {
	mirror := newFakeMirror(t, true)
	handlerWaiting := make(chan struct{}, 8)
	fetcher := &blockingFetcher{entered: make(chan struct{}, 16)}
	fixture := startFixture(t, fixtureOptions{
		cfg:       Config{Upstreams: loopbackConfig(mirror).Upstreams},
		internals: internals{fetcher: fetcher, handlerWaiting: handlerWaiting},
	}, mirror)

	requestCount := cap(fixture.server.handlers) + 1
	results := make([]<-chan fetchResult, 0, requestCount)
	for i := 0; i < requestCount; i++ {
		results = append(results, fixture.getAsync(fmt.Sprintf("/packages/a/b/file-%02d.whl", i)))
	}
	awaitSignal(t, handlerWaiting, "request queued before relay close")
	if err := fixture.server.Close(); err != nil {
		t.Fatalf("Close() error = %v, want queued handlers to exit", err)
	}
	for i, pending := range results {
		result := awaitResult(t, pending, "request after relay close")
		if result.err == nil && result.status != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502 after relay close", i, result.status)
		}
	}
}
