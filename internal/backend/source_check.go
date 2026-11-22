package backend

import (
	"context"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

// 后台检查使用本轮监督日志，必须在该日志关闭前退出。
type sourceCheckLog struct {
	Logger
	stop func()
}

func (l sourceCheckLog) Close() error { l.stop(); return l.Logger.Close() }

func (s *ManagedSupervisor) startSourceCheck(ctx context.Context, request Request, commit string, logger Logger) Logger {
	if s.deps.VerifyPendingSource == nil {
		return logger
	}
	bind := context.WithCancel
	if request.Control != nil {
		bind = request.Control.WithTerminalCancellation
	}
	work, cancel := bind(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		dirty, err := s.deps.VerifyPendingSource(work, commit)
		if work.Err() != nil || (!dirty && err == nil) {
			return
		}
		message := "发现受管源码不一致，下次启动将离线复核和修复"
		if err != nil {
			message = "受管源码检查未完成：" + err.Error()
		}
		// 日志失败不影响待修复标记。
		_ = logger.Record(work, process.StreamRecord{Stream: "runtime", Fragment: message, EndOfLine: true})
	}()
	return sourceCheckLog{Logger: logger, stop: func() { cancel(); <-done }}
}
