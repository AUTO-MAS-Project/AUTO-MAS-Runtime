package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// chunk 是一段闭区间字节范围。
type chunk struct {
	start int64
	end   int64
}

func (c chunk) length() int64 { return c.end - c.start + 1 }

// chunkPlan 是一次分片取回的共享状态：待取片栈、本文件失败源与各源贡献，
// 以及慢源判定所需的进行中片进度（T14.8）。
type chunkPlan struct {
	mu            sync.Mutex // 保护以下全部字段。
	pending       []chunk
	failed        map[string]bool
	attempts      []AttemptOutcome
	contributions map[string]int64
	// completed 是本文件已完成的片数；inflight 记录各 worker 正在取的片；slow 是本文件内被判为慢源、
	// 不再分派新片的源。changed 在 pending / inflight 变化时关闭并换新，供等待方醒来。
	completed int
	inflight  map[string]*inflightChunk
	slow      map[string]bool
	changed   chan struct{}
	clock     func() time.Time
}

// inflightChunk 是一个 worker 正在取的片：开始时刻、开始时的全局完成片数、已收字节与取消入口。
type inflightChunk struct {
	piece            chunk
	startedAt        time.Time
	startedCompleted int
	received         int64
	cancel           context.CancelFunc
}

const (
	// slowSourceCompletedLead 是判慢的门槛：别的 worker 完成了这么多片、自己当前片还没过 slowSourceProgressLimit。
	slowSourceCompletedLead = 2
	// slowSourceProgressLimit 是判慢时自己当前片允许的最大进度（四分之一）。
	slowSourceProgressLimit = 4
	// tailLaggardProgressLimit 是收尾判慢的进度门槛（一半）：队列已空、别的 worker 至少完成一片而它还没过半。
	tailLaggardProgressLimit = 2
	// slowSourceMinAge 是判慢前该片至少要在飞的时长：首字节延迟、连接建立与调度抖动都不算慢，
	// 只有持续落后才算——否则 TTFB 400 ms 的官方源会在快源起跑时被误判。
	slowSourceMinAge = 1500 * time.Millisecond
)

func newChunkPlan(clock func() time.Time) *chunkPlan {
	if clock == nil {
		clock = time.Now
	}
	return &chunkPlan{
		clock:         clock,
		failed:        make(map[string]bool),
		contributions: make(map[string]int64),
		inflight:      make(map[string]*inflightChunk),
		slow:          make(map[string]bool),
		changed:       make(chan struct{}),
	}
}

// notifyLocked 唤醒所有等待 pending / inflight 变化的 worker；调用方须持锁。
func (p *chunkPlan) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// begin 登记 key 开始取 piece；返回可取消该片的 context。
func (p *chunkPlan) begin(ctx context.Context, key string, piece chunk) context.Context {
	chunkCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.inflight[key] = &inflightChunk{piece: piece, startedAt: p.clock(), startedCompleted: p.completed, cancel: cancel}
	p.mu.Unlock()
	return chunkCtx
}

// progress 记录 key 当前片新收到的字节。
func (p *chunkPlan) progress(key string, count int64) {
	p.mu.Lock()
	if entry, ok := p.inflight[key]; ok {
		entry.received += count
	}
	p.mu.Unlock()
}

// end 注销 key 的进行中片并释放取消入口。
func (p *chunkPlan) end(key string) {
	p.mu.Lock()
	if entry, ok := p.inflight[key]; ok {
		entry.cancel()
		delete(p.inflight, key)
	}
	p.notifyLocked()
	p.mu.Unlock()
}

// complete 记一片完成。
func (p *chunkPlan) complete() {
	p.mu.Lock()
	p.completed++
	p.mu.Unlock()
}

func (p *chunkPlan) isSlow(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slow[key]
}

// demoteLaggards 由刚完成一片的 worker 调用：别的 worker 自它开始当前片以来已有 slowSourceCompletedLead 片完成、
// 而它自己的片还不到 1/slowSourceProgressLimit，即判为慢源——取消它的片（剩余范围由它自己回队）并不再分派。
// 返回被降级的源 key。
func (p *chunkPlan) demoteLaggards(self string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	var demoted []string
	for key, entry := range p.inflight {
		if key == self || p.slow[key] {
			continue
		}
		if p.completed-entry.startedCompleted < slowSourceCompletedLead {
			continue
		}
		if entry.received*slowSourceProgressLimit >= entry.piece.length() {
			continue
		}
		if now.Sub(entry.startedAt) < slowSourceMinAge {
			continue
		}
		p.slow[key] = true
		entry.cancel()
		demoted = append(demoted, key)
	}
	return demoted
}

// demoteTailLaggard 由队列已空、准备退出的 worker 调用：若还有别的 worker 在取片、自本 worker 上一片开始后至少
// 完成过一片、且对方进度未过半，则判对方为慢源并取消其片，让本 worker 接手剩余范围。返回是否降级了谁。
func (p *chunkPlan) demoteTailLaggard(self string, selfCompleted bool) bool {
	if !selfCompleted {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	for key, entry := range p.inflight {
		if key == self || p.slow[key] {
			continue
		}
		if entry.received*tailLaggardProgressLimit >= entry.piece.length() {
			continue
		}
		if now.Sub(entry.startedAt) < slowSourceMinAge {
			continue
		}
		p.slow[key] = true
		entry.cancel()
		return true
	}
	return false
}

// awaitChange 等待 pending / inflight 发生变化，或 ctx 结束；用于降级后等待慢源把剩余范围回队。
func (p *chunkPlan) awaitChange(ctx context.Context) bool {
	p.mu.Lock()
	changed := p.changed
	p.mu.Unlock()
	select {
	case <-changed:
		return true
	case <-ctx.Done():
		return false
	}
}

// othersInflight 报告除 self 之外是否还有 worker 在取片。
func (p *chunkPlan) othersInflight(self string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.inflight {
		if key != self {
			return true
		}
	}
	return false
}

func (p *chunkPlan) pop() (chunk, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return chunk{}, false
	}
	last := len(p.pending) - 1
	next := p.pending[last]
	p.pending = p.pending[:last]
	return next, true
}

func (p *chunkPlan) push(piece chunk) {
	p.mu.Lock()
	p.pending = append(p.pending, piece)
	p.notifyLocked()
	p.mu.Unlock()
}

func (p *chunkPlan) remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func (p *chunkPlan) markFailed(key string, outcome Outcome) {
	p.mu.Lock()
	p.failed[key] = true
	p.attempts = append(p.attempts, AttemptOutcome{Source: key, Outcome: outcome})
	p.mu.Unlock()
}

func (p *chunkPlan) credit(key string, count int64) {
	p.mu.Lock()
	p.contributions[key] += count
	p.mu.Unlock()
}

// rangeSources 返回该路由中支持 Range 且本文件尚未失败的源；有非慢源时不再分派给本文件内已判慢的源。
func (e *engine) rangeSources(route Route, plan *chunkPlan) []Upstream {
	var sources, fast []Upstream
	plan.mu.Lock()
	failed, slow := plan.failed, plan.slow
	for _, upstream := range e.snapshotUpstreams(route) {
		if !upstream.AcceptRange || failed[upstream.Key] {
			continue
		}
		sources = append(sources, upstream)
		if !slow[upstream.Key] {
			fast = append(fast, upstream)
		}
	}
	plan.mu.Unlock()
	if len(fast) > 0 {
		return fast
	}
	return sources
}

// tryChunked 在文件够大且至少两个源支持 Range 时分片并行取回；返回 true 表示 part 已完整且哈希通过。
// 返回 false 时调用方按剩余未失败的源退化为单流从头取；分片阶段的失败源与结局都写回 plan。
func (e *engine) tryChunked(ctx context.Context, spec fileSpec, part string, plan *chunkPlan) bool {
	if spec.size < e.cfg.ChunkThreshold || spec.size <= 0 {
		return false
	}
	if len(e.rangeSources(spec.key.route, plan)) < 2 {
		return false
	}
	file, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		e.log("warning", "relay staging open failed", map[string]any{"item": spec.name, "error": err.Error()})
		return false
	}
	defer func() {
		// 只在成功路径之外关闭；成功路径下面显式关闭并检查错误。
		_ = file.Close()
	}()
	if err := file.Truncate(spec.size); err != nil {
		e.log("warning", "relay staging truncate failed", map[string]any{"item": spec.name, "error": err.Error()})
		return false
	}
	for start := int64(0); start < spec.size; start += e.cfg.ChunkSize {
		end := start + e.cfg.ChunkSize - 1
		if end >= spec.size {
			end = spec.size - 1
		}
		plan.pending = append(plan.pending, chunk{start: start, end: end})
	}
	// 栈顶先出：倒序压入让 worker 从文件头开始取。
	for i, j := 0, len(plan.pending)-1; i < j; i, j = i+1, j-1 {
		plan.pending[i], plan.pending[j] = plan.pending[j], plan.pending[i]
	}

	for plan.remaining() > 0 && ctx.Err() == nil {
		sources := e.rangeSources(spec.key.route, plan)
		if len(sources) == 0 {
			break
		}
		if len(sources) > e.cfg.MaxChunksPerFile {
			sources = sources[:e.cfg.MaxChunksPerFile]
		}
		var workers sync.WaitGroup
		for _, upstream := range sources {
			workers.Add(1)
			go func(upstream Upstream) {
				defer workers.Done()
				e.chunkWorker(ctx, spec, upstream, file, plan)
			}(upstream)
		}
		workers.Wait()
	}
	if plan.remaining() > 0 {
		e.tracker.reset(spec.key)
		return false
	}
	if err := file.Sync(); err != nil {
		e.log("warning", "relay staging sync failed", map[string]any{"item": spec.name, "error": err.Error()})
		return false
	}
	if !verifyDigest(file, spec.digest) {
		// 分片拼接后无法归因到单个源：所有参与源对本文件失败，但不计入剔除。
		plan.mu.Lock()
		for key := range plan.contributions {
			plan.failed[key] = true
			plan.attempts = append(plan.attempts, AttemptOutcome{Source: key, Outcome: OutcomeChecksumMismatch})
			delete(plan.contributions, key)
		}
		plan.mu.Unlock()
		e.tracker.reset(spec.key)
		e.log("warning", "relay chunked checksum mismatch", map[string]any{"item": spec.name})
		return false
	}
	return true
}

// chunkWorker 绑定一个源逐片取回；任一片失败即回队、标记该源失败并退出。
//
// 慢源处理（T14.8，限速 200 KB/s 一类的源）：每完成一片就检查别的 worker 是否明显落后，落后者被判慢、
// 当前片被取消，它已写下的字节保留、剩余范围回队交给快源；队列空了但别人还在慢慢取时，快 worker
// 同样可以接手对方剩余范围，而不是干等最后一片。
func (e *engine) chunkWorker(
	ctx context.Context,
	spec fileSpec,
	upstream Upstream,
	file *os.File,
	plan *chunkPlan,
) {
	selfCompleted := false
	for ctx.Err() == nil {
		piece, ok := plan.pop()
		if !ok {
			if plan.isSlow(upstream.Key) || !plan.othersInflight(upstream.Key) {
				return
			}
			if !plan.demoteTailLaggard(upstream.Key, selfCompleted) {
				return
			}
			// 被降级的 worker 会把剩余范围回队；等它回队（或退出）后再取。
			if !plan.awaitChange(ctx) {
				return
			}
			continue
		}
		chunkCtx := plan.begin(ctx, upstream.Key, piece)
		received, outcome := e.fetchChunk(chunkCtx, spec, upstream, piece, file, plan)
		plan.end(upstream.Key)
		if outcome == "" {
			plan.credit(upstream.Key, received)
			plan.complete()
			selfCompleted = true
			for _, demoted := range plan.demoteLaggards(upstream.Key) {
				e.demoteUpstream(spec.key.route, demoted)
				e.log("info", "relay slow source demoted", map[string]any{
					"route": spec.key.route.String(), "item": spec.name, "source": demoted, "by": upstream.Key,
				})
			}
			continue
		}
		if plan.isSlow(upstream.Key) && ctx.Err() == nil {
			// 被判慢：已写下的字节有效并计入贡献，剩余范围回队；本 worker 退出，不算失败。
			plan.credit(upstream.Key, received)
			if received < piece.length() {
				plan.push(chunk{start: piece.start + received, end: piece.end})
			}
			e.demoteUpstream(spec.key.route, upstream.Key)
			return
		}
		plan.push(piece)
		e.tracker.discard(spec.key, upstream.Key, received)
		plan.markFailed(upstream.Key, outcome)
		e.log("warning", "relay chunk failed", map[string]any{
			"route": spec.key.route.String(), "item": spec.name,
			"source": upstream.Key, "outcome": outcome, "start": piece.start,
		})
		return
	}
}

// fetchChunk 以 Range 取一片写入 file 的对应偏移；上游不按 206 回应或长度不符都算该源失败。
func (e *engine) fetchChunk(
	ctx context.Context,
	spec fileSpec,
	upstream Upstream,
	piece chunk,
	file *os.File,
	plan *chunkPlan,
) (int64, Outcome) {
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", piece.start, piece.end)}
	handle, outcome := e.open(ctx, http.MethodGet, upstream.Base+spec.key.path, headers)
	if outcome != "" {
		return 0, outcome
	}
	defer handle.close()
	response := handle.response
	if response.StatusCode != http.StatusPartialContent {
		return 0, OutcomeHTTPStatus
	}
	if response.ContentLength >= 0 && response.ContentLength != piece.length() {
		return 0, OutcomeNetwork
	}
	writer := io.NewOffsetWriter(file, piece.start)
	received, outcome := e.readBody(ctx, handle, writer, piece.length(), func(count int64) {
		e.tracker.add(spec.key, spec.name, upstream.Key, count)
		plan.progress(upstream.Key, count)
	})
	if outcome != "" {
		return received, outcome
	}
	if received != piece.length() {
		return received, OutcomeNetwork
	}
	return received, ""
}

// verifyDigest 从头读一遍拼好的文件计算 sha256；digest 为空时视为通过。
func verifyDigest(file *os.File, digest []byte) bool {
	if digest == nil {
		return true
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(hasher.Sum(nil), digest) == 1
}
