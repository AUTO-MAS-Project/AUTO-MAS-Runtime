//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const guardianArgument = "--internal-posix-guardian-v1"

type guardianSpec struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
}
type guardianEvent struct {
	PID      int
	ExitCode int
	Error    string
}

// RunInternalGuardian 仅在专用继承管道齐备时进入内部守护模式。
// 守护进程不接收协议 stdout，存活管道 EOF 后负责回收同 session 的普通后代。
func RunInternalGuardian() (bool, int) {
	if len(os.Args) != 2 || os.Args[1] != guardianArgument {
		return false, 0
	}
	life := os.NewFile(3, "guardian-life")
	status := os.NewFile(4, "guardian-status")
	stdout := os.NewFile(5, "backend-stdout")
	stderr := os.NewFile(6, "backend-stderr")
	for _, file := range []*os.File{life, status, stdout, stderr} {
		if file == nil {
			return true, 125
		}
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return true, 125
		}
	}
	for _, file := range []*os.File{life, status, stdout, stderr} {
		unix.CloseOnExec(int(file.Fd()))
	}
	defer life.Close()
	defer status.Close()
	defer stdout.Close()
	defer stderr.Close()
	var spec guardianSpec
	if err := json.NewDecoder(io.LimitReader(life, 1<<20)).Decode(&spec); err != nil {
		return true, 125
	}
	encoder := json.NewEncoder(status)
	cmd := exec.CommandContext(context.Background(), spec.Executable, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = encoder.Encode(guardianEvent{Error: err.Error()})
		return true, 125
	}
	_ = stdout.Close()
	_ = stderr.Close()
	if err := encoder.Encode(guardianEvent{PID: cmd.Process.Pid}); err != nil {
		killGuardianSession()
		return true, 125
	}
	lost := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, life); close(lost) }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		code := 0
		if err != nil {
			code = cmd.ProcessState.ExitCode()
		}
		_ = encoder.Encode(guardianEvent{PID: cmd.Process.Pid, ExitCode: code})
		<-lost
	case <-lost:
	}
	killGuardianSession()
	return true, 0
}

func killGuardianSession() {
	// 自身 PID 固定 session 身份，清理期间不会把复用的 PID 当作受管边界。
	sid := os.Getpid()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err := sessionMembers(sid)
		if err == nil && len(members) == 0 {
			return
		}
		for _, member := range members {
			if current, e := unix.Getsid(int(member.PID)); e == nil && current == sid {
				_ = unix.Kill(int(member.PID), unix.SIGKILL)
			}
		}
		select {
		case <-deadline.C:
			// 守护进程自身仍锚定主进程组；最后一次组终止同时回收本组中新建的后代。
			_ = unix.Kill(-sid, unix.SIGKILL)
			return
		case <-ticker.C:
		}
	}
}

type posixManagedJob struct {
	guardian *exec.Cmd
	life     *os.File
	session  int
	identity string
	done     chan struct{}
	once     sync.Once
	closeErr error
	mu       sync.Mutex
	waitErr  error
}

func (*posixManagedJob) Assign(uint32) error             { return ErrUnsupported }
func (*posixManagedJob) assignProcess(*os.Process) error { return ErrUnsupported }
func (j *posixManagedJob) snapshot() ([]Info, error) {
	probe := state.NewSystemPIDProbe()
	alive, err := probe.Alive(context.Background(), uint32(j.session))
	if err != nil {
		return nil, err
	}
	if alive {
		identity, err := probe.Identity(context.Background(), uint32(j.session))
		if err != nil {
			return nil, err
		}
		if identity != j.identity {
			return nil, errors.New("guardian process identity changed")
		}
	}
	return sessionMembers(j.session)
}
func (j *posixManagedJob) Terminate(uint32) error {
	members, err := j.snapshot()
	if err != nil {
		return err
	}
	var result error
	for _, member := range members {
		sid, e := unix.Getsid(int(member.PID))
		if errors.Is(e, unix.ESRCH) {
			continue
		}
		if e != nil {
			result = errors.Join(result, e)
			continue
		}
		if sid != j.session {
			continue
		}
		e = unix.Kill(int(member.PID), unix.SIGKILL)
		if !errors.Is(e, unix.ESRCH) {
			result = errors.Join(result, e)
		}
	}
	return result
}
func (j *posixManagedJob) waitEmpty(ctx context.Context) error {
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		members, err := j.snapshot()
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (j *posixManagedJob) Close() error {
	j.once.Do(func() {
		j.closeErr = j.life.Close()
		select {
		case <-j.done:
			j.mu.Lock()
			j.closeErr = errors.Join(j.closeErr, j.waitErr)
			j.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			j.closeErr = errors.Join(j.closeErr, j.Terminate(1), j.waitEmpty(ctx))
		case <-time.After(5 * time.Second):
			j.closeErr = errors.Join(j.closeErr, errors.New("guardian cleanup timed out"))
		}
	})
	return j.closeErr
}

// StartManaged 先启动独立 session 的守护进程，再经专用管道交付后端启动参数。
func StartManaged(ctx context.Context, spec StartSpec) (*ManagedProcess, error) {
	if ctx == nil || !filepath.IsAbs(spec.Executable) || !filepath.IsAbs(spec.Dir) || spec.Env == nil || (spec.Sink != nil && spec.ReadStream != nil) {
		return nil, errors.New("managed process arguments are invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var resources []*os.File
	pipe := func() (*os.File, *os.File, error) {
		r, w, e := os.Pipe()
		if e == nil {
			resources = append(resources, r, w)
		}
		return r, w, e
	}
	cleanup := func() {
		for _, file := range resources {
			_ = file.Close()
		}
	}
	lifeR, lifeW, err := pipe()
	if err != nil {
		return nil, err
	}
	statusR, statusW, err := pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	outR, outW, err := pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), executable, guardianArgument)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{lifeR, statusW, outW, errW}
	// 不继承入口的标准流；后端日志只写专用管道。
	if err = cmd.Start(); err != nil {
		cleanup()
		return nil, err
	}
	_ = lifeR.Close()
	_ = statusW.Close()
	_ = outW.Close()
	_ = errW.Close()
	j := &posixManagedJob{guardian: cmd, life: lifeW, session: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		j.mu.Lock()
		j.waitErr = err
		j.mu.Unlock()
		close(j.done)
		if err != nil {
			_ = j.Terminate(1)
		}
	}()
	fail := func(err error) (*ManagedProcess, error) {
		_ = lifeW.Close()
		_ = statusR.Close()
		_ = outR.Close()
		_ = errR.Close()
		return nil, errors.Join(err, j.Close())
	}
	j.identity, err = state.NewSystemPIDProbe().Identity(ctx, uint32(j.session))
	if err != nil {
		return fail(err)
	}
	stopCancelledStart := context.AfterFunc(ctx, func() { _ = lifeW.Close(); _ = statusR.Close() })
	defer stopCancelledStart()
	_ = lifeW.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = statusR.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = json.NewEncoder(lifeW).Encode(guardianSpec{Executable: spec.Executable, Args: spec.Args, Dir: spec.Dir, Env: spec.Env}); err != nil {
		return fail(err)
	}
	decoder := json.NewDecoder(statusR)
	var started guardianEvent
	if err = decoder.Decode(&started); err != nil {
		return fail(err)
	}
	if started.PID <= 0 || started.Error != "" {
		return fail(errors.New("guardian could not start backend: " + started.Error))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	_ = lifeW.SetWriteDeadline(time.Time{})
	_ = statusR.SetReadDeadline(time.Time{})
	p := &ManagedProcess{job: j, pid: uint32(started.PID), exited: make(chan struct{}), waitDone: make(chan struct{}), closed: make(chan struct{})}
	sinkCtx, cancelSink := context.WithCancel(context.WithoutCancel(ctx))
	var readers sync.WaitGroup
	readersDone := make(chan struct{})
	readers.Add(2)
	sink := p.guardedSink(spec.Sink)
	drain := func(name string, file *os.File) {
		defer readers.Done()
		if spec.ReadStream != nil {
			if err := spec.ReadStream(sinkCtx, name, file); err != nil && !errors.Is(err, os.ErrClosed) {
				p.recordSinkError(err)
			}
		} else {
			drainStream(sinkCtx, name, file, sink, func(err error) {
				if !errors.Is(err, os.ErrClosed) {
					p.recordSinkError(err)
				}
			})
		}
		_ = file.Close()
	}
	go drain(StreamStdout, outR)
	go drain(StreamStderr, errR)
	go func() { readers.Wait(); close(readersDone) }()
	var stop sync.Once
	p.stopReaders = func() {
		stop.Do(func() {
			timer := time.NewTimer(managedSinkShutdownTimeout)
			defer timer.Stop()
			select {
			case <-readersDone:
				return
			case <-timer.C:
			}
			cancelSink()
			_ = outR.Close()
			_ = errR.Close()
		})
	}
	go func() {
		var exited guardianEvent
		err := decoder.Decode(&exited)
		_ = statusR.Close()
		p.mu.Lock()
		p.result = ExitResult{ExitCode: exited.ExitCode}
		p.waitErr = err
		p.mu.Unlock()
		close(p.exited)
		readers.Wait()
		close(p.waitDone)
	}()
	// 取消和 Close 都有明确退出路径，根进程退出不提前丢失后代监督。
	go func() {
		select {
		case <-ctx.Done():
			p.recordCancellation(ctx.Err())
			_ = p.Terminate(130)
			p.stopReaders()
		case <-p.closed:
			p.stopReaders()
		}
	}()
	return p, nil
}

// ResumeSuspended 在 POSIX 上无需恢复；进程组在 exec 前由内核设置。
func ResumeSuspended(*os.Process) error { return nil }
