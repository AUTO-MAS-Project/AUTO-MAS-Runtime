package backend

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestBackend_HealthTimeoutSurvivesShutdownDuringCleanup(t *testing.T) {
	f := newBackendFixture(t)
	f.proc.keepAlive = true
	f.proc.cleanupStarted = make(chan struct{})
	f.proc.cleanupRelease = make(chan struct{})
	f.health.err = newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendHealth, "后端健康检查超时", nil, nil)
	mailbox := NewControlMailbox(8)
	done := make(chan error, 1)
	go func() {
		request := f.request()
		request.Control = mailbox
		done <- f.supervisor().Supervise(t.Context(), request)
	}()
	waitFor(t, f.proc.cleanupStarted)
	if err := mailbox.Submit(context.Background(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "late-shutdown"}); err != nil {
		t.Fatal(err)
	}
	close(f.proc.cleanupRelease)
	select {
	case err := <-done:
		assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("startup cleanup did not settle")
	}
	if f.uv.startCalls != 1 {
		t.Fatalf("spawned %d backends", f.uv.startCalls)
	}
}

// TestBackend_StartupTimeoutPrecedesElectron 依据 C22 检查前置启动耗时是否计入预算。
// 沿用监督器夹具，只在外部进程启动边界注入 35 秒耗时，HTTP 始终有效应答 running。
func TestBackend_StartupTimeoutPrecedesElectron(t *testing.T) {
	f := newBackendFixture(t)
	clock := &startupBudgetClock{now: time.Unix(0, 0)}
	f.uv.onStart = func() { clock.advance(35 * time.Second) }
	supervisor := f.supervisor()
	supervisor.deps.Clock = clock.Now
	supervisor.deps.Health = health.NewChecker(health.Config{
		Transport: runningHealthTransport{},
		Clock:     clock,
	})
	startedAt := clock.Now()
	err := supervisor.Supervise(t.Context(), f.request())
	assertBackendCode(t, err, protocol.CodeBackendHealthTimeout)
	elapsed := clock.Now().Sub(startedAt)
	t.Logf("supervise elapsed = %s, preflight/spawn = 35s, code = %s", elapsed, protocol.CodeBackendHealthTimeout)
	if elapsed >= 180*time.Second {
		t.Fatalf("Runtime timeout at %s, want before Electron's 180s deadline", elapsed)
	}
}

type runningHealthTransport struct{}

func (runningHealthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body := `{"ready":false,"backgroundStatus":"running","backgroundError":null,"protocol":1,"version":"v1.2.3","commit":"` + strings.Repeat("a", 40) + `"}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// startupBudgetClock 仅在创建轮询计时器时推进时间，全部已到期计时器一起触发。
type startupBudgetClock struct {
	mu sync.Mutex
	// now 和 timers 由 mu 保护。
	now    time.Time
	timers []*startupBudgetTimer
}

func (c *startupBudgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *startupBudgetClock) NewTimer(duration time.Duration) health.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &startupBudgetTimer{clock: c, deadline: c.now.Add(duration), channel: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	if duration == 200*time.Millisecond {
		c.advanceLocked(duration)
	}
	return timer
}

func (c *startupBudgetClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(duration)
}

func (c *startupBudgetClock) advanceLocked(duration time.Duration) {
	c.now = c.now.Add(duration)
	for _, timer := range c.timers {
		if !timer.done && !timer.deadline.After(c.now) {
			timer.done = true
			timer.channel <- c.now
		}
	}
}

type startupBudgetTimer struct {
	clock    *startupBudgetClock
	deadline time.Time
	channel  chan time.Time
	// done 由 clock.mu 保护。
	done bool
}

func (t *startupBudgetTimer) C() <-chan time.Time { return t.channel }

func (t *startupBudgetTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasLive := !t.done
	t.done = true
	return wasLive
}
