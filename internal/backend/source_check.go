package backend

import (
	"context"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

// sourceCheckLog 把后台读取的生命周期绑定到本次监督尝试；关闭日志前收回任务。
type sourceCheckLog struct {
	Logger
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (l *sourceCheckLog) Close() error {
	l.cancel()
	<-l.done
	return l.Logger.Close()
}

func (s *ManagedSupervisor) startSourceCheck(ctx context.Context, commit string, logger Logger) Logger {
	verify := s.deps.VerifyPendingSource
	if verify == nil {
		return logger
	}
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		dirty, err := verify(work, commit)
		if work.Err() != nil {
			return
		}
		message := "本轮受管源码检查结束"
		if err != nil {
			message = "受管源码检查未完成：" + err.Error()
		} else if dirty {
			message = "发现受管源码不一致，下次启动将离线复核和修复"
		}
		_ = logger.Record(work, process.StreamRecord{Stream: "runtime", Fragment: message, EndOfLine: true})
	}()
	return &sourceCheckLog{Logger: logger, cancel: cancel, done: done}
}
