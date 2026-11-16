package cli

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

// sessionDiagnostics 只观察协议投影，独立持有本地日志，不回调 Emitter。
type sessionDiagnostics struct {
	protocol.EventRenderer
	deps *deps
	// mu 保护日志器与计时快照；renderer 和终态收口共用该顺序。
	mu             sync.Mutex
	logger         *logging.Logger
	command        string
	operationID    string
	runtimeVersion string
	started        time.Time
	stage          protocol.Stage
	stageStarted   time.Time
	openAttempted  bool
	ended          bool
}

func (s *sessionDiagnostics) RenderHello(event protocol.HelloEvent) error {
	if err := s.EventRenderer.RenderHello(event); err != nil {
		return err
	}
	s.observe(func() {
		s.command, s.operationID, s.runtimeVersion = event.Command, event.OperationID, event.RuntimeVersion
		s.started = s.deps.options.clock()
	})
	return nil
}

func sessionWritesLogs(command string) bool {
	switch command {
	case "bootstrap", "workspace sync", "workspace stage", "environment ensure", "environment repair", "dependencies sync", "dependencies rebuild", "backend supervise", "repair", "cleanup":
		return true
	default:
		return false
	}
}

func (s *sessionDiagnostics) RenderProgress(event protocol.ProgressEvent) error {
	if err := s.EventRenderer.RenderProgress(event); err != nil {
		return err
	}
	s.observe(func() { s.changeStage(event.Stage) })
	return nil
}

func (s *sessionDiagnostics) RenderState(event protocol.StateEvent) error {
	if err := s.EventRenderer.RenderState(event); err != nil {
		return err
	}
	s.observe(func() {
		s.changeStage(event.Stage)
		s.record("state.change", map[string]any{"stage": string(event.Stage), "status": string(event.Status), "sequence": event.Sequence})
	})
	return nil
}

func (s *sessionDiagnostics) RenderResult(event protocol.ResultEvent) error {
	err := s.EventRenderer.RenderResult(event)
	s.observe(func() {
		s.ended = true
		if (event.Success && sessionWritesLogs(s.command)) || (!event.Success && sessionRecordsFailure(protocol.Code(event.Code))) {
			s.open()
		}
		s.finishStage(s.deps.options.clock())
		s.record("operation.end", map[string]any{
			"success": event.Success, "code": event.Code, "stage": string(event.Stage), "status": event.Status,
			"durationMs": elapsedMilliseconds(s.started, s.deps.options.clock()), "terminalWritten": err == nil,
			"summary": telemetry.SanitizeDiagnostics(event.Details),
		})
	})
	return err
}

func (s *sessionDiagnostics) finish(exitCode int) {
	if s == nil {
		return
	}
	s.observe(func() {
		if s.ended || s.logger == nil {
			return
		}
		s.ended = true
		s.finishStage(s.deps.options.clock())
		s.record("operation.end", map[string]any{"success": false, "exitCode": exitCode, "terminalWritten": false,
			"durationMs": elapsedMilliseconds(s.started, s.deps.options.clock())})
	})
}

func (s *sessionDiagnostics) open() {
	if s.openAttempted || s.operationID == "" {
		return
	}
	s.openAttempted = true
	ctx, cancel := context.WithTimeout(context.Background(), m5TransactionCleanupTimeout)
	defer cancel()
	logger, err := logging.New(ctx, s.deps.global.layout, io.Discard, s.command, s.operationID, logging.WithClock(s.deps.options.clock))
	if err != nil {
		return
	}
	s.logger = logger
	s.record("operation.start", map[string]any{"runtimeVersion": s.runtimeVersion, "protocolVersion": protocol.Version, "startedAt": s.started.Format(time.RFC3339Nano)})
}

func (s *sessionDiagnostics) changeStage(stage protocol.Stage) {
	if sessionWritesLogs(s.command) {
		s.open()
	}
	if s.stage == stage {
		return
	}
	now := s.deps.options.clock()
	s.finishStage(now)
	s.stage, s.stageStarted = stage, now
}

func sessionRecordsFailure(code protocol.Code) bool {
	switch code {
	case protocol.CodeInvalidArgument, protocol.CodeInvalidVersion, protocol.CodeUnsupportedMode:
		return false
	default:
		return true
	}
}

func (s *sessionDiagnostics) finishStage(now time.Time) {
	if s.stage == "" {
		return
	}
	s.record("stage.end", map[string]any{"stage": string(s.stage), "durationMs": elapsedMilliseconds(s.stageStarted, now)})
	s.stage = ""
}

func elapsedMilliseconds(start, end time.Time) int64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func (s *sessionDiagnostics) record(operation string, details map[string]any) {
	if s.logger == nil {
		return
	}
	details["operation"] = operation
	ctx, cancel := context.WithTimeout(context.Background(), m5TransactionCleanupTimeout)
	defer cancel()
	// 会话观察失败不能遮盖业务结果，业务日志仍保留原有错误处理。
	_, _ = s.logger.Record(ctx, logging.LevelInfo, "运行时操作诊断", details)
}

func (s *sessionDiagnostics) observe(run func()) {
	defer func() { _ = recover() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	run()
}

func (s *sessionDiagnostics) close() {
	if s == nil {
		return
	}
	s.observe(func() {
		if s.logger != nil {
			// 附加观察日志不改变退出码，日志器负责尽力关闭全部句柄。
			_ = s.logger.Close()
		}
	})
}
