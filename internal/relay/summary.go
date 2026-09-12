package relay

import "sync"

// ledger 累计交付结果与失败明细，供 Summary 快照。
type ledger struct {
	mu       sync.Mutex // 保护以下全部字段。
	files    int
	bytes    int64
	bySource map[string]int64
	failures []FileFailure
}

func newLedger() *ledger {
	return &ledger{bySource: make(map[string]int64)}
}

// delivered 记录一个已交付文件；contributions 是各源贡献的字节。
func (l *ledger) delivered(size int64, contributions map[string]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files++
	l.bytes += size
	for source, count := range contributions {
		l.bySource[source] += count
	}
}

// failed 记录一个所有源都失败的文件；超过 maxFailures 的条目只计数不保留。
func (l *ledger) failed(item string, attempts []AttemptOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) >= maxFailures {
		return
	}
	l.failures = append(l.failures, FileFailure{
		Item:     item,
		Attempts: append([]AttemptOutcome(nil), attempts...),
	})
}

func (l *ledger) summary() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	bySource := make(map[string]int64, len(l.bySource))
	for source, count := range l.bySource {
		bySource[source] = count
	}
	failures := make([]FileFailure, 0, len(l.failures))
	for _, failure := range l.failures {
		failures = append(failures, FileFailure{
			Item:     failure.Item,
			Attempts: append([]AttemptOutcome(nil), failure.Attempts...),
		})
	}
	return Summary{
		Files:    l.files,
		Bytes:    l.bytes,
		BySource: bySource,
		Failures: failures,
	}
}
