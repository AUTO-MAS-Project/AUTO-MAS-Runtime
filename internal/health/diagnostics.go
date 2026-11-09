package health

import (
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// checkDiagnostics 只在 Check 所属 goroutine 内更新，不保存原始响应或错误文本。
type checkDiagnostics struct {
	startedAt    time.Time
	port         int
	attemptCount int
	failureKind  string
	operation    string
	state        string
}

func (d *checkDiagnostics) request(result requestResult) {
	d.attemptCount++
	d.operation = "request"
	switch result.kind {
	case requestTransportError:
		d.failureKind = mirror.NetworkFailureKind(result.transportErr).String()
	case requestTimedOut:
		d.failureKind = "request_timeout"
	default:
		d.failureKind = "request_incomplete"
	}
}

func (d *checkDiagnostics) details(now time.Time) map[string]any {
	details := map[string]any{
		"attemptCount": d.attemptCount, "durationMs": max(int64(0), now.Sub(d.startedAt).Milliseconds()),
		"port": d.port, "failureKind": d.failureKind,
	}
	if d.operation != "" {
		details["operation"] = d.operation
	}
	if d.state != "" {
		details["state"] = d.state
	}
	return details
}
