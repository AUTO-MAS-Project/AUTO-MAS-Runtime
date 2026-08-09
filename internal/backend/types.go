package backend

import (
	"context"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// EventEmitter 是后端监督使用的窄事件出口；顶层 error/result 由 CLI 统一发出。
type EventEmitter interface {
	EmitState(protocol.StateEvent) error
	EmitLog(protocol.LogEvent) error
}

// Request 描述一次受管后端监督请求。
type Request struct {
	OperationID string
	RuntimePID  uint32
	Emitter     EventEmitter
}

// Dependencies 是 ManagedSupervisor 的消费侧依赖集合。
type Dependencies struct {
	Lock        LockSet
	State       StateStore
	Repository  RepositoryChecker
	Entry       EntryChecker
	UV          UVRunner
	Health      HealthChecker
	Logger      LoggerFactory
	Clock       func() time.Time
	UVPath      string
	PythonPath  string
	PythonPaths []string
	PID         PIDProbe
}

// LockSet 提供后端 Mutex 的零等待租约。
type LockSet interface {
	Acquire(context.Context) (Lease, error)
	Close() error
}

// Lease 是一次后端 Mutex 租约。
type Lease interface {
	Close() error
}

// StateStore 是后端消费的稳定环境与事务存储窄接口。
type StateStore interface {
	ReadEnvironment(context.Context) (state.EnvironmentState, error)
	ReadBackendTransaction(context.Context) (Transaction, error)
	BeginBackendTransaction(context.Context, TransactionInput) (TransactionHandle, error)
	UpdateBackendTransaction(context.Context, TransactionHandle, protocol.Stage) error
	RemoveBackendTransaction(context.Context, TransactionHandle) error
	Close() error
}

// Transaction 描述已有 backend 事务的最小事实。
type Transaction struct {
	PID     uint32
	Version string
	Stage   protocol.Stage
	Handle  TransactionHandle
}

// TransactionInput 描述新建 backend 事务的业务身份。
type TransactionInput struct {
	OperationID string
	PID         uint32
	Version     string
	Stage       protocol.Stage
}

// TransactionHandle 是条件删除 backend 事务所需的不可伪造 token。
type TransactionHandle interface{}

// PIDProbe 判断事务记录对应的旧监督进程是否仍存活。
type PIDProbe interface {
	Alive(context.Context, uint32) (bool, error)
}

// ErrTransactionNotFound 表示当前没有 backend 事务。
var ErrTransactionNotFound = errTransactionNotFound{}

type errTransactionNotFound struct{}

func (errTransactionNotFound) Error() string { return "backend transaction not found" }

// RepositoryChecker 返回经验证的活动仓库 revision。
type RepositoryChecker interface {
	Check(context.Context) (RepositoryResult, error)
}

// RepositoryResult 是 backend 需要的仓库检查结果。
type RepositoryResult struct {
	Healthy bool
	Version string
	Commit  string
	Reason  string
}

// EntryChecker 验证受管 backend 入口文件的普通文件与 reparse 身份。
type EntryChecker interface {
	Check(context.Context, string) error
}

// UVRunner 是长驻受管 uv 的唯一启动入口。
type UVRunner interface {
	Check(context.Context) error
	Executable() string
	StartManaged(context.Context, []string, uv.ManagedOptions, process.StreamSink) (ManagedProcess, error)
}

// ManagedProcess 是 backend 需要的进程生命周期能力。
type ManagedProcess interface {
	PID() uint32
	Exited() <-chan struct{}
	Wait(context.Context) (process.ExitResult, error)
	Snapshot() ([]process.Info, error)
	Terminate(uint32) error
	WaitEmpty(context.Context) error
	Close() error
}

// HealthChecker 执行已注入的后端健康与身份检查。
type HealthChecker interface {
	Check(context.Context, health.Expectation, health.Probe) error
}

// Logger 记录受管流并提供日志路径与收口操作。
type Logger interface {
	Record(context.Context, process.StreamRecord) error
	LogPath() string
	Close() error
}

// LoggerFactory 延迟创建本次监督操作的日志 sink。
type LoggerFactory func(context.Context, Request) (Logger, error)
