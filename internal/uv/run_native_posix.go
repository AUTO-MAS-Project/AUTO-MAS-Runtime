//go:build linux || darwin

package uv

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type nativeRunResult struct {
	result UVResult
	err    error
}

func (r *UVRunner) runNative(ctx context.Context, args []string, options RunOptions, resolved resolvedRunOptions) (nativeRunResult, bool) {
	// 注入的故障路径仍走原有执行器，生产调用统一经宿主死亡守护。
	if r.newJob != nil || r.resumeProcess != nil {
		return nativeRunResult{}, false
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	started := clock()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	callbackCtx, cancelCallback := context.WithCancel(runCtx)
	defer cancelCallback()
	stdout, stderr := make(chan streamResult, 1), make(chan streamResult, 1)
	var managed *process.ManagedProcess
	ready := make(chan struct{})
	stop := func() {
		cancel()
		<-ready
		if managed != nil {
			// 输出已超限或回调失败时同步终止；最终 WaitEmpty 仍会验证回收事实。
			_ = managed.Terminate(1)
		}
	}
	callback := func(stream, line string) error {
		if err := callbackCtx.Err(); err != nil {
			return err
		}
		if resolved.Line == nil {
			return nil
		}
		err := resolved.Line(callbackCtx, stream, line)
		if err != nil {
			cancelCallback()
			stop()
		}
		return err
	}
	var err error
	managed, err = process.StartManaged(runCtx, process.StartSpec{Executable: r.Executable, Args: args, Dir: resolved.WorkingDir, Env: buildEnvironment(resolved), ReadStream: func(streamCtx context.Context, stream string, reader io.Reader) error {
		stopCallback := context.AfterFunc(streamCtx, cancelCallback)
		defer stopCallback()
		result := stdout
		if stream == process.StreamStderr {
			result = stderr
		}
		readUVStream(stream, reader, callback, stop, result)
		return nil
	}})
	close(ready)
	if err != nil {
		return nativeRunResult{err: newError(protocol.CodeUVExecFailed, options.Stage, "uv 执行失败", startFailureDetails(resolved, err), err)}, true
	}
	// 根进程退出后先回收普通后代，再排空管道；取消也始终执行同一有界清理路径。
	select {
	case <-managed.Exited():
	case <-runCtx.Done():
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanupCancel()
	cleanupErr := managed.Terminate(1)
	cleanupErr = errors.Join(cleanupErr, managed.WaitEmpty(cleanupCtx))
	exit, waitErr := managed.Wait(cleanupCtx)
	cleanupErr = errors.Join(cleanupErr, managed.Close())
	out, errs := <-stdout, <-stderr
	result := UVResult{ExitCode: exit.ExitCode, Stdout: out.output, Stderr: errs.output, Duration: clock().Sub(started)}
	streamErr := errors.Join(out.err, errs.err, waitErr, cleanupErr)
	if err := ctx.Err(); err != nil {
		return nativeRunResult{result: result, err: runnerCancellationError(options.Stage, runnerDetails(result, resolved), err)}, true
	}
	if streamErr != nil {
		return nativeRunResult{result: result, err: newError(protocol.CodeUVExecFailed, options.Stage, "uv 输出读取失败", runnerFailureDetails(result, resolved, "output_read_failed"), streamErr)}, true
	}
	if result.ExitCode != 0 {
		return nativeRunResult{result: result, err: newError(protocol.CodeUVExecFailed, options.Stage, "uv 执行失败", runnerFailureDetails(result, resolved, "nonzero_exit"), nil)}, true
	}
	return nativeRunResult{result: result}, true
}
