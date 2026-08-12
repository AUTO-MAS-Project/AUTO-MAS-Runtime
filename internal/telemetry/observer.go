package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// InternalObservation 是允许发送到 Sentry 的内部错误分类。
type InternalObservation struct {
	Command         string
	Stage           string
	Code            string
	RuntimeVersion  string
	ProtocolVersion int
	Platform        string
	Panic           bool
	PanicFrames     []StackFrame
}

// StackFrame 是允许附带到 panic 观测的最小栈帧；不包含源文件路径或局部变量。
type StackFrame struct {
	Function string
	Module   string
	Lineno   int
}

// Recorder 是 CLI 消费的最小遥测接口；实现不得把错误返回给业务调用方。
type Recorder interface {
	CaptureInternal(context.Context, InternalObservation)
	Close(context.Context)
}

type provider interface {
	captureInternal(InternalObservation)
	close(context.Context)
}

type providerFactory func(Config) (provider, error)

var errProviderFactoryPanic = errors.New("telemetry provider factory panicked")

const codeNone = "NONE"

var knownCommands = map[string]struct{}{
	"version":              {},
	"doctor":               {},
	"bootstrap":            {},
	"workspace check":      {},
	"workspace sync":       {},
	"environment check":    {},
	"environment ensure":   {},
	"environment repair":   {},
	"dependencies check":   {},
	"dependencies sync":    {},
	"dependencies rebuild": {},
	"backend supervise":    {},
	"repair":               {},
	"cleanup":              {},
}

// Observer 汇总多个 provider，并把关闭生命周期线性化。
type Observer struct {
	mu           sync.RWMutex
	providerMu   sync.Mutex
	closed       bool
	closeOnce    sync.Once
	closeDone    chan struct{}
	flushTimeout time.Duration
	providers    []provider
}

// New 根据配置构造遥测观察器；禁用、离线或缺少凭据时不会创建 SDK client。
func New(config Config) Recorder {
	return newObserverWithFactory(config, newSentryProvider)
}

func newObserverWithFactory(config Config, sentryFactory providerFactory) *Observer {
	config = config.normalized()
	observer := &Observer{
		closeDone:    make(chan struct{}),
		flushTimeout: config.FlushTimeout,
	}
	if !config.Enabled || config.Offline {
		return observer
	}
	if config.SentryDSN != "" && validSentryDSN(config.SentryDSN) && sentryFactory != nil {
		if candidate, err := safeProviderFactory(sentryFactory, config); err == nil && candidate != nil {
			observer.providers = append(observer.providers, candidate)
		}
	}
	return observer
}

func safeProviderFactory(factory providerFactory, config Config) (candidate provider, err error) {
	defer func() {
		if recover() != nil {
			candidate = nil
			err = errProviderFactoryPanic
		}
	}()
	return factory(config)
}

// CaptureInternal 记录允许上报的内部错误分类，不携带原始错误文本。
func (o *Observer) CaptureInternal(ctx context.Context, observation InternalObservation) {
	if o == nil || ctx == nil || ctx.Err() != nil || !validInternalObservation(observation) {
		return
	}
	o.mu.RLock()
	if o.closed {
		o.mu.RUnlock()
		return
	}
	providers := append([]provider(nil), o.providers...)
	o.mu.RUnlock()
	o.providerMu.Lock()
	defer o.providerMu.Unlock()
	o.mu.RLock()
	closed := o.closed
	o.mu.RUnlock()
	if closed {
		return
	}
	for _, candidate := range providers {
		safeProviderCaptureInternal(candidate, observation)
	}
}

// Close 在给定上下文截止前尽力收口全部 provider，重复调用安全。
func (o *Observer) Close(ctx context.Context) {
	if o == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	if o.closeDone == nil {
		o.closeDone = make(chan struct{})
	}
	closeDone := o.closeDone
	o.mu.Unlock()
	closeContext, cancel := context.WithTimeout(ctx, o.flushDuration())
	defer cancel()
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closed = true
		providers := append([]provider(nil), o.providers...)
		o.mu.Unlock()
		go func() {
			o.providerMu.Lock()
			defer o.providerMu.Unlock()
			for _, candidate := range providers {
				safeProviderClose(candidate, closeContext)
			}
			close(closeDone)
		}()
	})
	select {
	case <-closeDone:
	case <-closeContext.Done():
	}
}

func validInternalObservation(observation InternalObservation) bool {
	if !observation.Panic && len(observation.PanicFrames) > 0 {
		return false
	}
	if len(observation.PanicFrames) > 64 {
		return false
	}
	for _, frame := range observation.PanicFrames {
		if frame.Lineno < 0 || (frame.Function != "" && !validTelemetryText(frame.Function, 256)) ||
			(frame.Module != "" && !validTelemetryText(frame.Module, 256)) {
			return false
		}
	}
	return validCommand(observation.Command) &&
		protocol.IsKnownStage(protocol.Stage(observation.Stage)) &&
		observation.Code == string(protocol.CodeInternalError) &&
		validRuntimeVersion(observation.RuntimeVersion) && validPlatform(observation.Platform) &&
		observation.ProtocolVersion == protocol.Version
}

func validCommand(value string) bool {
	_, ok := knownCommands[value]
	return ok
}

func validRuntimeVersion(value string) bool {
	value = strings.TrimSpace(value)
	if value == "dev" {
		return true
	}
	for _, prefix := range []string{"auto-mas-runtime@", "runtime@"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	if len(value) < 2 || value[0] != 'v' || value[1] < '0' || value[1] > '9' {
		return false
	}
	for _, r := range value[2:] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '+') {
			return false
		}
	}
	return len(value) <= 128
}

func validSentryEnvironment(value string) bool {
	switch value {
	case "production", "staging", "development", "testing":
		return true
	default:
		return false
	}
}

func validPlatform(value string) bool {
	if len(value) > 64 || strings.Count(value, "/") != 1 {
		return false
	}
	parts := strings.SplitN(value, "/", 2)
	return validStableToken(parts[0], 32, true) && validStableToken(parts[1], 32, true)
}

func validStableToken(value string, maxLength int, lowerASCIIOnly bool) bool {
	if value == "" || len(value) > maxLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '\\' || r == ':' {
			return false
		}
		if lowerASCIIOnly && !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validTelemetryText(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\\' || r == ':' {
			return false
		}
	}
	return true
}

func safeProviderClose(candidate provider, ctx context.Context) {
	defer func() { _ = recover() }()
	if candidate == nil {
		return
	}
	candidate.close(ctx)
}

func safeProviderCaptureInternal(candidate provider, observation InternalObservation) {
	defer func() { _ = recover() }()
	if candidate != nil {
		candidate.captureInternal(observation)
	}
}

func (o *Observer) flushDuration() time.Duration {
	o.mu.RLock()
	duration := o.flushTimeout
	o.mu.RUnlock()
	if duration <= 0 || duration > DefaultFlushTimeout {
		return DefaultFlushTimeout
	}
	return duration
}
