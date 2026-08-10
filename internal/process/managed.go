package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const managedSinkShutdownTimeout = time.Second

const (
	// StreamStdout 标识受管进程的标准输出。
	StreamStdout = "stdout"
	// StreamStderr 标识受管进程的标准错误。
	StreamStderr = "stderr"
)

// StartSpec 描述一个必须先加入 Job 才能恢复执行的进程。
type StartSpec struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
	Sink       StreamSink
}

// StreamRecord 是交给日志层的不可变流片段。
type StreamRecord struct {
	Stream        string
	LineID        uint64
	Fragment      string
	EndOfLine     bool
	Event         string
	Truncated     bool
	OriginalBytes int64
}

// StreamSink 按每个流的原始顺序接收规范化文本片段，并必须在 ctx 取消后有界返回。
type StreamSink func(ctx context.Context, record StreamRecord) error

// Info 描述 Job 内一个进程的稳定诊断身份。
type Info struct {
	PID        uint32
	ParentPID  uint32
	Executable string
}

// ExitResult 保存 uv 根进程的退出事实。
type ExitResult struct {
	ExitCode int
}

type managedJob interface {
	Job
	assignProcess(*os.Process) error
	snapshot() ([]Info, error)
	waitEmpty(context.Context) error
}

// ManagedProcess 持有根进程、Job 和两个输出 reader 的唯一生命周期。
type ManagedProcess struct {
	process *os.Process
	job     managedJob
	pid     uint32

	exited   chan struct{}
	waitDone chan struct{}
	closed   chan struct{}

	// mu 保护最终退出结果、取消原因和首个 sink 错误。
	mu        sync.Mutex
	result    ExitResult
	waitErr   error
	cancelErr error
	sinkErr   error
	// sinkMu 保证两个流不会并发调用 sink，并在首错后停止后续调用。
	sinkMu      sync.Mutex
	sinkStopped bool
	closeOnce   sync.Once
	closeError  error
}

func (p *ManagedProcess) guardedSink(sink StreamSink) StreamSink {
	if sink == nil {
		return nil
	}
	return func(ctx context.Context, record StreamRecord) error {
		p.sinkMu.Lock()
		defer p.sinkMu.Unlock()
		if p.sinkStopped {
			return nil
		}
		err := sink(ctx, record)
		if err != nil {
			p.sinkStopped = true
		}
		return err
	}
}

// PID 返回 Runtime 直接监督的 uv 根进程 PID。
func (p *ManagedProcess) PID() uint32 {
	if p == nil {
		return 0
	}
	return p.pid
}

// Exited 在根进程退出后立即关闭，不等待仍持有管道的后代。
func (p *ManagedProcess) Exited() <-chan struct{} {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.exited
}

// Wait 等待根进程与两路 reader 全部收口，并缓存稳定结果。
func (p *ManagedProcess) Wait(ctx context.Context) (ExitResult, error) {
	if p == nil {
		return ExitResult{}, errors.New("managed process is nil")
	}
	if ctx == nil {
		return ExitResult{}, errors.New("managed process wait context is nil")
	}
	select {
	case <-p.waitDone:
		return p.cachedResult()
	default:
	}
	select {
	case <-p.waitDone:
		return p.cachedResult()
	case <-ctx.Done():
		return ExitResult{}, ctx.Err()
	}
}

func (p *ManagedProcess) cachedResult() (ExitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result, errors.Join(p.sinkErr, p.waitErr, p.cancelErr)
}

// Snapshot 返回仍属于本 Job 的进程树快照。
func (p *ManagedProcess) Snapshot() ([]Info, error) {
	if p == nil || p.job == nil {
		return nil, errors.New("managed process job is unavailable")
	}
	return p.job.snapshot()
}

// Terminate 终止 Job 内全部进程，但保留 Job handle 供 WaitEmpty 证明。
func (p *ManagedProcess) Terminate(exitCode uint32) error {
	if p == nil || p.job == nil {
		return errors.New("managed process job is unavailable")
	}
	return p.job.Terminate(exitCode)
}

// WaitEmpty 在 Job handle 仍有效时确认进程树为空。
func (p *ManagedProcess) WaitEmpty(ctx context.Context) error {
	if p == nil || p.job == nil {
		return errors.New("managed process job is unavailable")
	}
	if ctx == nil {
		return errors.New("managed process wait-empty context is nil")
	}
	return p.job.waitEmpty(ctx)
}

// Close 幂等关闭 Job handle；调用前应先用 WaitEmpty 证明树为空。
func (p *ManagedProcess) Close() error {
	if p == nil || p.job == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closeError = p.job.Close()
		close(p.closed)
	})
	return p.closeError
}

func (p *ManagedProcess) recordSinkError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	first := p.sinkErr == nil
	if first {
		p.sinkErr = err
	}
	p.mu.Unlock()
	if first {
		if terminateErr := p.job.Terminate(97); terminateErr != nil {
			p.mu.Lock()
			p.waitErr = errors.Join(p.waitErr, fmt.Errorf("terminate managed process after sink failure: %w", terminateErr))
			p.mu.Unlock()
		}
	}
}

func (p *ManagedProcess) recordCancellation(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.cancelErr == nil {
		p.cancelErr = err
	}
	p.mu.Unlock()
}

func processExitResult(state *os.ProcessState, err error) (ExitResult, error) {
	if err != nil {
		return ExitResult{ExitCode: -1}, fmt.Errorf("wait managed process: %w", err)
	}
	if state == nil {
		return ExitResult{ExitCode: -1}, errors.New("wait managed process returned no state")
	}
	return ExitResult{ExitCode: state.ExitCode()}, nil
}
