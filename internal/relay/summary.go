package relay

import "sync"

// ledger 累计交付结果与失败明细，供 Summary 快照。
type ledger struct {
	mu            sync.Mutex // 保护以下全部字段。
	files         int
	bytes         int64
	bySource      map[string]int64
	failures      []FileFailure
	indexFailures []FileFailure
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
func (l *ledger) failed(item string, attempts []AttemptOutcome, diagnosticOnly ...bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	only := len(diagnosticOnly) > 0 && diagnosticOnly[0]
	failures := &l.failures
	if only {
		failures = &l.indexFailures
	}
	if len(*failures) >= maxFailures {
		return
	}
	*failures = append(*failures, FileFailure{
		Item:           item,
		Attempts:       append([]AttemptOutcome(nil), attempts...),
		DiagnosticOnly: only,
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
	// 制品失败始终优先，新增索引诊断不能挤掉旧错误码判断所需的事实。
	allFailures := append(append([]FileFailure(nil), l.failures...), l.indexFailures...)
	for _, failure := range allFailures {
		if len(failures) >= maxFailures {
			break
		}
		failures = append(failures, FileFailure{
			Item:           failure.Item,
			Attempts:       append([]AttemptOutcome(nil), failure.Attempts...),
			DiagnosticOnly: failure.DiagnosticOnly,
		})
	}
	return Summary{
		Files:    l.files,
		Bytes:    l.bytes,
		BySource: bySource,
		Failures: failures,
	}
}
