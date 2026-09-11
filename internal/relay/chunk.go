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
)

// chunk 是一段闭区间字节范围。
type chunk struct {
	start int64
	end   int64
}

func (c chunk) length() int64 { return c.end - c.start + 1 }

// chunkPlan 是一次分片取回的共享状态：待取片栈、本文件失败源与各源贡献。
type chunkPlan struct {
	mu            sync.Mutex // 保护以下全部字段。
	pending       []chunk
	failed        map[string]bool
	attempts      []AttemptOutcome
	contributions map[string]int64
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

// rangeSources 返回该路由中支持 Range 且本文件尚未失败的源。
func (e *engine) rangeSources(route Route, failed map[string]bool) []Upstream {
	var sources []Upstream
	for _, upstream := range e.snapshotUpstreams(route) {
		if upstream.AcceptRange && !failed[upstream.Key] {
			sources = append(sources, upstream)
		}
	}
	return sources
}

// tryChunked 在文件够大且至少两个源支持 Range 时分片并行取回；返回 true 表示 part 已完整且哈希通过。
// 返回 false 时调用方按剩余未失败的源退化为单流从头取；分片阶段的失败源与结局都写回 plan。
func (e *engine) tryChunked(ctx context.Context, spec fileSpec, part string, plan *chunkPlan) bool {
	if spec.size < e.cfg.ChunkThreshold || spec.size <= 0 {
		return false
	}
	if len(e.rangeSources(spec.key.route, plan.failed)) < 2 {
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
		sources := e.rangeSources(spec.key.route, plan.failed)
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
func (e *engine) chunkWorker(
	ctx context.Context,
	spec fileSpec,
	upstream Upstream,
	file *os.File,
	plan *chunkPlan,
) {
	for {
		piece, ok := plan.pop()
		if !ok {
			return
		}
		received, outcome := e.fetchChunk(ctx, spec, upstream, piece, file)
		if outcome == "" {
			plan.credit(upstream.Key, received)
			continue
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
