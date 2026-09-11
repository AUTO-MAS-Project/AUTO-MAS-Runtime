package relay

import (
	"sync"
	"time"
)

// fileKey 唯一标识一次取回中的文件。
type fileKey struct {
	route Route
	path  string
}

type sample struct {
	at    time.Time
	bytes int64
}

// tracker 聚合全部取回的字节并按节流规则同步回调：
// 每 progressInterval 至多一次且只在有变化时回调，文件完成 / 失败时强制一次；
// BytesPerSecond 取最近 throughputWindow 窗口内的字节。
type tracker struct {
	clock    func() time.Time
	callback func(Progress) error
	onStop   func()

	mu        sync.Mutex // 保护以下全部字段。
	stopped   bool
	total     int64
	delivered int64
	inflight  map[fileKey]int64
	perSource map[fileKey]map[string]int64
	current   Progress
	samples   []sample
	reported  bool
	lastAt    time.Time
	last      Progress
}

func newTracker(
	clock func() time.Time,
	callback func(Progress) error,
	onStop func(),
) *tracker {
	return &tracker{
		clock:     clock,
		callback:  callback,
		onStop:    onStop,
		inflight:  make(map[fileKey]int64),
		perSource: make(map[fileKey]map[string]int64),
	}
}

// addTotal 把新得知的文件大小并入总量；不立即回调，随下一次字节到达一起报告。
func (t *tracker) addTotal(size int64) {
	if size <= 0 {
		return
	}
	t.mu.Lock()
	t.total += size
	t.mu.Unlock()
}

// add 记录某文件从某源新收到的字节；分片并行时当前源报贡献最多的那个。
func (t *tracker) add(key fileKey, item, source string, count int64) {
	if count <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight[key] += count
	sources := t.perSource[key]
	if sources == nil {
		sources = make(map[string]int64)
		t.perSource[key] = sources
	}
	sources[source] += count
	t.current = Progress{Route: key.route, Item: item, Source: topSource(sources)}
	t.samples = append(t.samples, sample{at: t.clock(), bytes: count})
	t.report(false)
}

// discard 撤回某源一片失败前已计入的字节。
func (t *tracker) discard(key fileKey, source string, count int64) {
	if count <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight[key] -= count
	if sources := t.perSource[key]; sources != nil {
		sources[source] -= count
	}
}

// reset 在换源从头取时清掉该文件的进行中字节。
func (t *tracker) reset(key fileKey) {
	t.mu.Lock()
	t.inflight[key] = 0
	delete(t.perSource, key)
	t.mu.Unlock()
}

// complete 把文件从进行中转为已交付并强制回调一次。
func (t *tracker) complete(key fileKey, item, source string, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, key)
	delete(t.perSource, key)
	t.delivered += size
	t.current = Progress{Route: key.route, Item: item, Source: source}
	t.report(true)
}

// fail 丢弃文件的进行中字节并强制回调一次。
func (t *tracker) fail(key fileKey, item string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, key)
	delete(t.perSource, key)
	t.current = Progress{Route: key.route, Item: item, Source: t.current.Source}
	t.report(true)
}

func (t *tracker) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// report 在持有 mu 的前提下应用节流并同步回调；回调失败即停止中继。
func (t *tracker) report(force bool) {
	if t.callback == nil || t.stopped {
		return
	}
	now := t.clock()
	snapshot := t.snapshotLocked(now)
	if !force && t.reported {
		if now.Sub(t.lastAt) < progressInterval || sameProgress(snapshot, t.last) {
			return
		}
	}
	if err := t.callback(snapshot); err != nil {
		t.stopped = true
		if t.onStop != nil {
			t.onStop()
		}
		return
	}
	t.reported = true
	t.lastAt = now
	t.last = snapshot
}

func (t *tracker) snapshotLocked(now time.Time) Progress {
	var inflight int64
	for _, count := range t.inflight {
		inflight += count
	}
	return Progress{
		Route:          t.current.Route,
		Item:           t.current.Item,
		Source:         t.current.Source,
		Received:       t.delivered + inflight,
		Total:          t.total,
		BytesPerSecond: t.throughputLocked(now),
	}
}

// throughputLocked 丢弃窗口外的样本并求和；样本按时间追加，因此只需从头裁剪。
func (t *tracker) throughputLocked(now time.Time) int64 {
	cutoff := now.Add(-throughputWindow)
	kept := 0
	for kept < len(t.samples) && !t.samples[kept].at.After(cutoff) {
		kept++
	}
	if kept > 0 {
		t.samples = append(t.samples[:0], t.samples[kept:]...)
	}
	var sum int64
	for _, entry := range t.samples {
		sum += entry.bytes
	}
	return sum
}

func sameProgress(left, right Progress) bool {
	return left.Route == right.Route && left.Item == right.Item &&
		left.Source == right.Source && left.Received == right.Received &&
		left.Total == right.Total
}
