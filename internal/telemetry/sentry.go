package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	defaultSentryRelease = "auto-mas-runtime"
	sentryServerName     = "auto-mas-runtime"
	maxSentryStackFrames = 64

	sentryObservationTag      = "_auto_mas_observation"
	sentryObservationInternal = "internal_error"
	sentryObservationPanic    = "panic"

	sentryInternalMessage = "runtime internal error"
	sentryPanicMessage    = "runtime panic"
	sentryExceptionType   = "auto_mas_runtime"
)

// sentryProvider 将稳定的内部错误分类转换为经净化的 Sentry 事件。
type sentryProvider struct {
	client       *sentry.Client
	hub          *sentry.Hub
	flushTimeout time.Duration

	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeDone chan struct{}
}

// newSentryProvider 创建官方 Sentry Go SDK client；所有无效或关闭配置由上层静默丢弃。
func newSentryProvider(config Config) (provider, error) {
	return newSentryProviderWithTransport(config, nil)
}

// newSentryProviderWithTransport 允许测试注入同步 fake transport，生产路径使用 SDK 默认 HTTP transport。
func newSentryProviderWithTransport(config Config, transport sentry.Transport) (provider, error) {
	config = config.normalized()
	if !config.Enabled || config.Offline || config.SentryDSN == "" {
		return nil, nil
	}
	if !validSentryDSN(config.SentryDSN) {
		return nil, errors.New("invalid sentry dsn")
	}

	release := config.SentryRelease
	if !validRuntimeVersion(release) {
		release = defaultSentryRelease
	}
	environment := config.SentryEnvironment
	if !validSentryEnvironment(environment) {
		environment = defaultSentryEnvironment
	}

	options := sentry.ClientOptions{
		Dsn:                    config.SentryDSN,
		Release:                release,
		Environment:            environment,
		SendDefaultPII:         false,
		EnableTracing:          false,
		DisableLogs:            true,
		DisableMetrics:         true,
		DisableClientReports:   true,
		DisableTelemetryBuffer: true,
		ServerName:             sentryServerName,
		DataCollection: &sentry.DataCollection{
			UserInfo: sentry.Set(false),
			Cookies:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			HTTPHeaders: &sentry.HeaderCollectionConfig{
				Request:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
				Response: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			},
			HTTPBodies:  []sentry.BodyType{},
			QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
		},
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return sanitizeSentryEvent(event, release, environment)
		},
	}
	if transport != nil {
		options.Transport = transport
	} else {
		httpClient, err := newSentryHTTPClient(config.SentryDSN, config.FlushTimeout)
		if err != nil {
			return nil, err
		}
		options.HTTPClient = httpClient
	}

	client, err := sentry.NewClient(options)
	if err != nil {
		return nil, err
	}

	return &sentryProvider{
		client:       client,
		hub:          sentry.NewHub(client, sentry.NewScope()),
		flushTimeout: config.FlushTimeout,
		closeDone:    make(chan struct{}),
	}, nil
}

func (p *sentryProvider) captureInternal(observation InternalObservation) {
	if p == nil || !validInternalObservation(observation) {
		return
	}

	kind := sentryObservationInternal
	level := sentry.LevelError
	message := sentryInternalMessage
	if observation.Panic {
		kind = sentryObservationPanic
		level = sentry.LevelFatal
		message = sentryPanicMessage
	}

	event := sentry.NewEvent()
	event.Level = level
	event.Message = message
	event.Tags = map[string]string{
		sentryObservationTag: kind,
		"command":            observation.Command,
		"stage":              observation.Stage,
		"code":               observation.Code,
		"runtime_version":    observation.RuntimeVersion,
		"protocol_version":   strconv.Itoa(observation.ProtocolVersion),
		"platform":           observation.Platform,
	}
	stacktrace := sentry.NewStacktrace()
	if len(observation.PanicFrames) > 0 {
		frames := make([]sentry.Frame, 0, len(observation.PanicFrames))
		for _, frame := range observation.PanicFrames {
			frames = append(frames, sentry.Frame{
				Function: frame.Function,
				Module:   frame.Module,
				Lineno:   frame.Lineno,
			})
		}
		stacktrace = &sentry.Stacktrace{Frames: frames}
	}
	event.Exception = []sentry.Exception{{
		Type:       sentryExceptionType,
		Value:      message,
		Stacktrace: stacktrace,
	}}
	if observation.Panic {
		mechanism := &sentry.Mechanism{Type: "panic"}
		mechanism.SetUnhandled()
		event.Exception[0].Mechanism = mechanism
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed || p.hub == nil {
		return
	}
	p.hub.CaptureEvent(event)
}

func (p *sentryProvider) close(ctx context.Context) {
	if p == nil || p.client == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.closeDone == nil {
		p.closeDone = make(chan struct{})
	}
	closeDone := p.closeDone
	p.mu.Unlock()
	p.closeOnce.Do(func() {
		defer close(closeDone)
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()

		flushTimeout := p.flushTimeout
		if flushTimeout <= 0 || flushTimeout > DefaultFlushTimeout {
			flushTimeout = DefaultFlushTimeout
		}
		flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
		defer cancel()
		func() {
			defer func() { _ = recover() }()
			_ = p.client.FlushWithContext(flushCtx)
			p.client.Close()
		}()
	})
	<-closeDone
}

func sanitizeSentryEvent(event *sentry.Event, release, environment string) *sentry.Event {
	if event == nil {
		return nil
	}
	kind, ok := event.Tags[sentryObservationTag]
	if !ok || (kind != sentryObservationInternal && kind != sentryObservationPanic) {
		return nil
	}
	if release == "" {
		release = defaultSentryRelease
	}
	if environment == "" {
		environment = defaultSentryEnvironment
	}

	message := sentryInternalMessage
	level := sentry.LevelError
	mechanism := (*sentry.Mechanism)(nil)
	if kind == sentryObservationPanic {
		message = sentryPanicMessage
		level = sentry.LevelFatal
		mechanism = &sentry.Mechanism{Type: "panic"}
		mechanism.SetUnhandled()
	}

	sanitized := &sentry.Event{
		EventID:     event.EventID,
		Timestamp:   event.Timestamp,
		Platform:    "go",
		Sdk:         event.Sdk,
		Level:       level,
		Message:     message,
		Release:     release,
		Environment: environment,
		Tags:        sanitizeSentryTags(event.Tags),
		Exception: []sentry.Exception{{
			Type:       sentryExceptionType,
			Value:      message,
			Module:     "auto-mas-runtime",
			Stacktrace: sanitizeSentryStacktrace(firstSentryStacktrace(event.Exception)),
			Mechanism:  mechanism,
		}},
	}
	return sanitized
}

func sanitizeSentryTags(tags map[string]string) map[string]string {
	allowed := [...]string{"command", "stage", "code", "runtime_version", "protocol_version", "platform"}
	clean := make(map[string]string, len(allowed))
	for _, key := range allowed {
		value := strings.TrimSpace(tags[key])
		if validSentryTag(key, value) {
			clean[key] = value
		}
	}
	return clean
}

func validSentryTag(key, value string) bool {
	if !safeSentryValue(value) {
		return false
	}
	switch key {
	case "command":
		return validCommand(value)
	case "stage":
		return protocol.IsKnownStage(protocol.Stage(value))
	case "code":
		return value == codeNone || protocol.IsKnownCode(protocol.Code(value))
	case "runtime_version":
		return validRuntimeVersion(value)
	case "platform":
		return validPlatform(value)
	case "protocol_version":
		version, err := strconv.Atoi(value)
		return err == nil && version == protocol.Version
	default:
		return false
	}
}

func safeSentryValue(value string) bool {
	if !validTelemetryText(value, 256) || strings.HasPrefix(value, "/") {
		return false
	}
	return true
}

func firstSentryStacktrace(exceptions []sentry.Exception) *sentry.Stacktrace {
	for _, exception := range exceptions {
		if exception.Stacktrace != nil {
			return exception.Stacktrace
		}
	}
	return nil
}

func sanitizeSentryStacktrace(stacktrace *sentry.Stacktrace) *sentry.Stacktrace {
	if stacktrace == nil {
		return nil
	}
	capacity := len(stacktrace.Frames)
	if capacity > maxSentryStackFrames {
		capacity = maxSentryStackFrames
	}
	frames := make([]sentry.Frame, 0, capacity)
	for _, frame := range stacktrace.Frames {
		if len(frames) == maxSentryStackFrames {
			break
		}
		function := sanitizeSentryStackValue(frame.Function)
		module := sanitizeSentryStackValue(frame.Module)
		line := frame.Lineno
		if line < 0 {
			line = 0
		}
		if function == "" && module == "" && line == 0 {
			continue
		}
		frames = append(frames, sentry.Frame{Function: function, Module: module, Lineno: line})
	}
	if len(frames) == 0 {
		return nil
	}
	return &sentry.Stacktrace{Frames: frames}
}

func sanitizeSentryStackValue(value string) string {
	value = strings.TrimSpace(value)
	if !safeSentryValue(value) {
		return ""
	}
	return value
}

func newSentryHTTPClient(rawDSN string, timeout time.Duration) (*http.Client, error) {
	dsn, err := sentry.NewDsn(rawDSN)
	if err != nil {
		return nil, errors.New("invalid sentry dsn")
	}
	endpoint := dsn.GetAPIURL()
	if endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Path == "" {
		return nil, errors.New("invalid sentry envelope endpoint")
	}
	if timeout <= 0 || timeout > DefaultFlushTimeout {
		timeout = DefaultFlushTimeout
	}
	return &http.Client{
		Transport: &sentryEndpointTransport{
			base:        defaultSentryTransport(),
			allowedHost: endpoint.Host,
			allowedPath: endpoint.Path,
		},
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func defaultSentryTransport() http.RoundTripper {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return http.DefaultTransport
}

// sentryEndpointTransport 只允许 SDK 向 DSN 推导出的 envelope 端点发送请求。
type sentryEndpointTransport struct {
	base        http.RoundTripper
	allowedHost string
	allowedPath string
}

func (t *sentryEndpointTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil {
		return nil, errors.New("sentry transport is unavailable")
	}
	if request == nil || request.URL == nil || request.Method != http.MethodPost ||
		request.URL.Scheme != "https" || request.URL.Path != t.allowedPath ||
		request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.Fragment != "" ||
		request.URL.ForceQuery || request.URL.Opaque != "" || request.URL.User != nil ||
		request.Body == nil || t.allowedHost == "" || t.allowedPath == "" ||
		!strings.EqualFold(request.URL.Host, t.allowedHost) ||
		request.Header.Get("Content-Type") != "application/x-sentry-envelope" {
		return nil, errors.New("sentry request is not allowed")
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("sentry transport returned an empty response")
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("sentry redirect is not allowed")
	}
	return response, nil
}
