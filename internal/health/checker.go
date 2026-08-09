package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	// HealthURL 是受管后端健康端点的固定地址。
	HealthURL = "http://127.0.0.1:36163/api/core/health"

	maxHealthBodyBytes          = 64 * 1024
	defaultTotalTimeout         = 60 * time.Second
	defaultPollInterval         = 500 * time.Millisecond
	defaultRequestTimeout       = 2 * time.Second
	defaultConsecutiveSuccesses = 2
)

// Mode 是健康检查的身份校验模式。
type Mode string

const (
	// ModeManaged 要求 version、commit 与 Runtime 期望值完全相等。
	ModeManaged Mode = "managed"
	// ModeDevelopment 只校验协议版本，不比较工作区身份。
	ModeDevelopment Mode = "development"
)

// Expectation 描述本次健康检查的受管身份期望。
type Expectation struct {
	Mode     Mode
	Protocol int
	Version  string
	Commit   string
}

// Probe 验证受管 Job 中的 uv/Python 身份和存活状态。
type Probe interface {
	Exited() <-chan struct{}
	// Healthy 必须尊重 ctx，并在返回前释放其临时快照资源。
	Healthy(context.Context) (bool, error)
}

// Transport 是可注入的 HTTP 请求执行器。
type Transport interface {
	// RoundTrip 必须尊重请求 context；返回的 Body 必须允许 Close 中断正在进行的 Read。
	RoundTrip(*http.Request) (*http.Response, error)
}

// Timer 是健康检查使用的可注入计时器。
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock 为健康检查提供当前时间和计时器。
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Config 配置健康检查的传输、时钟和预算。
type Config struct {
	Transport            Transport
	Clock                Clock
	TotalTimeout         time.Duration
	PollInterval         time.Duration
	RequestTimeout       time.Duration
	ConsecutiveSuccesses int
}

// Checker 轮询后端健康端点并执行协议、身份和 Job 探针校验。
type Checker struct {
	transport            Transport
	clock                Clock
	totalTimeout         time.Duration
	pollInterval         time.Duration
	requestTimeout       time.Duration
	consecutiveSuccesses int
}

// NewChecker 创建一个健康检查器；空配置项使用架构默认值。
func NewChecker(config Config) *Checker {
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
	}
	transport := config.Transport
	if transport == nil {
		transport = newDefaultTransport()
	}
	totalTimeout := config.TotalTimeout
	if totalTimeout <= 0 {
		totalTimeout = defaultTotalTimeout
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	consecutiveSuccesses := config.ConsecutiveSuccesses
	if consecutiveSuccesses <= 0 {
		consecutiveSuccesses = defaultConsecutiveSuccesses
	}
	return &Checker{
		transport:            transport,
		clock:                clock,
		totalTimeout:         totalTimeout,
		pollInterval:         pollInterval,
		requestTimeout:       requestTimeout,
		consecutiveSuccesses: consecutiveSuccesses,
	}
}

// Check 轮询后端直至连续满足健康条件或返回稳定失败分类。
func (c *Checker) Check(ctx context.Context, expected Expectation, probe Probe) error {
	if ctx == nil {
		return newError(protocol.CodeBackendHealthInvalid, "健康检查上下文不可用", nil, errors.New("health check context is nil"))
	}
	if err := cancellationError(ctx); err != nil {
		return err
	}
	if probe == nil {
		return newError(protocol.CodeBackendHealthInvalid, "后端进程探针不可用", nil, errors.New("health probe is unavailable"))
	}
	if err := validateExpectation(expected); err != nil {
		return err
	}

	exited := probe.Exited()
	totalTimer := c.clock.NewTimer(c.totalTimeout)
	defer totalTimer.Stop()
	total := totalTimer.C()
	startedAt := c.clock.Now()
	successes := 0
	for {
		if err := cancellationError(ctx); err != nil {
			return err
		}
		if c.clock.Now().Sub(startedAt) >= c.totalTimeout {
			return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
		}
		if channelClosed(exited) {
			return newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil)
		}

		request := c.request(ctx, total, exited)
		switch request.kind {
		case requestCancelled:
			if err := cancellationError(ctx); err != nil {
				return err
			}
			return newError(protocol.CodeOperationCancelled, "健康检查已取消", nil, context.Canceled)
		case requestExited:
			return preferCancellation(ctx, newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil))
		case requestTotalTimeout:
			if err := cancellationError(ctx); err != nil {
				return err
			}
			return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
		case requestTimedOut, requestTransportError:
			successes = 0
			if wait := c.wait(ctx, total, exited); wait != waitReady {
				return c.waitError(ctx, wait)
			}
			continue
		case requestResponse:
			if err := cancellationError(ctx); err != nil {
				return err
			}
			if c.totalExpired(total, startedAt) {
				return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
			}
			if channelClosed(exited) {
				return preferCancellation(ctx, newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil))
			}
			observation, err := inspectResponse(request.status, request.body, request.bodyErr, expected)
			if cancelErr := cancellationError(ctx); cancelErr != nil {
				return cancelErr
			}
			if err != nil {
				return err
			}
			if c.totalExpired(total, startedAt) {
				return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
			}
			if observation.continuePolling {
				successes = 0
				if wait := c.wait(ctx, total, exited); wait != waitReady {
					return c.waitError(ctx, wait)
				}
				continue
			}
			if channelClosed(exited) {
				return preferCancellation(ctx, newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil))
			}
			probeResult := c.probe(ctx, probe, total, exited)
			if err := cancellationError(ctx); err != nil {
				return err
			}
			if c.totalExpired(total, startedAt) {
				return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
			}
			switch probeResult.kind {
			case probeCancelled:
				if err := cancellationError(ctx); err != nil {
					return err
				}
				return newError(protocol.CodeOperationCancelled, "健康检查已取消", nil, context.Canceled)
			case probeExited:
				return preferCancellation(ctx, newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil))
			case probeTimeout:
				if err := cancellationError(ctx); err != nil {
					return err
				}
				return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
			case probeError:
				return preferCancellation(ctx, newError(protocol.CodeBackendHealthInvalid, "无法验证受管后端进程", nil, probeResult.err))
			case probeUnhealthy:
				return preferCancellation(ctx, newError(protocol.CodeBackendHealthInvalid, "受管后端进程身份无效", map[string]any{"reason": "job_probe_unhealthy"}, nil))
			case probeHealthy:
				successes++
				if successes >= c.consecutiveSuccesses {
					if err := cancellationError(ctx); err != nil {
						return err
					}
					if c.totalExpired(total, startedAt) {
						return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
					}
					return nil
				}
			}
			if wait := c.wait(ctx, total, exited); wait != waitReady {
				return c.waitError(ctx, wait)
			}
		}
	}
}

func (c *Checker) totalExpired(total <-chan time.Time, startedAt time.Time) bool {
	if c.clock.Now().Sub(startedAt) >= c.totalTimeout {
		return true
	}
	select {
	case <-total:
		return true
	default:
		return false
	}
}

func cancellationError(ctx context.Context) error {
	if cause := ctx.Err(); cause != nil {
		return newError(protocol.CodeOperationCancelled, "健康检查已取消", nil, cause)
	}
	return nil
}

func preferCancellation(ctx context.Context, fallback error) error {
	if err := cancellationError(ctx); err != nil {
		return err
	}
	return fallback
}

func timerReady(channel <-chan time.Time) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

type requestKind uint8

const (
	requestResponse requestKind = iota
	requestTransportError
	requestTimedOut
	requestCancelled
	requestTotalTimeout
	requestExited
)

type requestResult struct {
	kind         requestKind
	status       int
	body         []byte
	bodyErr      error
	transportErr error
}

func (c *Checker) request(ctx context.Context, total <-chan time.Time, exited <-chan struct{}) requestResult {
	requestContext, cancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, HealthURL, nil)
	if err != nil {
		cancel()
		return requestResult{kind: requestTransportError, transportErr: err}
	}
	done := make(chan requestResult, 1)
	go func() {
		response, roundTripErr := c.transport.RoundTrip(request)
		if roundTripErr != nil {
			closeResponse(response)
			done <- requestResult{kind: requestResponse, transportErr: roundTripErr}
			return
		}
		if response == nil {
			done <- requestResult{kind: requestResponse, bodyErr: errors.New("health response is nil")}
			return
		}
		if response.StatusCode != http.StatusOK {
			closeResponse(response)
			done <- requestResult{kind: requestResponse, status: response.StatusCode}
			return
		}
		body, bodyErr := readResponseBody(requestContext, response)
		done <- requestResult{kind: requestResponse, status: response.StatusCode, body: body, bodyErr: bodyErr}
	}()
	requestTimer := c.clock.NewTimer(c.requestTimeout)
	defer requestTimer.Stop()
	if ctx.Err() != nil {
		cancel()
		<-done
		return requestResult{kind: requestCancelled}
	}
	if channelClosed(exited) {
		cancel()
		<-done
		if ctx.Err() != nil {
			return requestResult{kind: requestCancelled}
		}
		return requestResult{kind: requestExited}
	}
	select {
	case <-ctx.Done():
		cancel()
		<-done
		return requestResult{kind: requestCancelled}
	case <-exited:
		cancel()
		<-done
		if ctx.Err() != nil {
			return requestResult{kind: requestCancelled}
		}
		return requestResult{kind: requestExited}
	case <-total:
		cancel()
		<-done
		if ctx.Err() != nil {
			return requestResult{kind: requestCancelled}
		}
		return requestResult{kind: requestTotalTimeout}
	case <-requestTimer.C():
		cancel()
		<-done
		if ctx.Err() != nil {
			return requestResult{kind: requestCancelled}
		}
		return requestResult{kind: requestTimedOut}
	case result := <-done:
		cancel()
		return completedRequestResult(ctx, exited, total, requestTimer.C(), result)
	}
}

func completedRequestResult(
	ctx context.Context,
	exited <-chan struct{},
	total <-chan time.Time,
	requestTimeout <-chan time.Time,
	result requestResult,
) requestResult {
	if ctx.Err() != nil {
		return requestResult{kind: requestCancelled}
	}
	if channelClosed(exited) {
		return requestResult{kind: requestExited}
	}
	if timerReady(total) {
		return requestResult{kind: requestTotalTimeout}
	}
	if timerReady(requestTimeout) {
		return requestResult{kind: requestTimedOut}
	}
	if result.transportErr != nil {
		return requestResult{kind: requestTransportError, transportErr: result.transportErr}
	}
	return result
}

type waitKind uint8

const (
	waitReady waitKind = iota
	waitCancelled
	waitExited
	waitTimeout
)

func (c *Checker) wait(ctx context.Context, total <-chan time.Time, exited <-chan struct{}) waitKind {
	timer := c.clock.NewTimer(c.pollInterval)
	defer timer.Stop()
	if cause := ctx.Err(); cause != nil {
		return waitCancelled
	}
	if channelClosed(exited) {
		return waitExited
	}
	select {
	case <-ctx.Done():
		return waitCancelled
	case <-exited:
		return waitExited
	case <-total:
		return waitTimeout
	case <-timer.C():
		return waitReady
	}
}

func (c *Checker) waitError(ctx context.Context, kind waitKind) error {
	if err := cancellationError(ctx); err != nil {
		return err
	}
	switch kind {
	case waitCancelled:
		return newError(protocol.CodeOperationCancelled, "健康检查已取消", nil, context.Canceled)
	case waitExited:
		return newError(protocol.CodeBackendExitedBeforeReady, "后端在就绪前退出", nil, nil)
	case waitTimeout:
		return newError(protocol.CodeBackendHealthTimeout, "后端健康检查超时", nil, nil)
	default:
		return nil
	}
}

type probeKind uint8

const (
	probeHealthy probeKind = iota
	probeUnhealthy
	probeError
	probeCancelled
	probeExited
	probeTimeout
)

type probeResult struct {
	kind probeKind
	err  error
}

func (c *Checker) probe(ctx context.Context, probe Probe, total <-chan time.Time, exited <-chan struct{}) probeResult {
	probeContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan probeResult, 1)
	go func() {
		healthy, err := probe.Healthy(probeContext)
		if err != nil {
			done <- probeResult{kind: probeError, err: err}
			return
		}
		if !healthy {
			done <- probeResult{kind: probeUnhealthy}
			return
		}
		done <- probeResult{kind: probeHealthy}
	}()
	select {
	case <-ctx.Done():
		cancel()
		<-done
		return probeResult{kind: probeCancelled, err: ctx.Err()}
	case <-exited:
		cancel()
		<-done
		if ctx.Err() != nil {
			return probeResult{kind: probeCancelled, err: ctx.Err()}
		}
		return probeResult{kind: probeExited}
	case <-total:
		cancel()
		<-done
		if ctx.Err() != nil {
			return probeResult{kind: probeCancelled, err: ctx.Err()}
		}
		return probeResult{kind: probeTimeout}
	case result := <-done:
		if cause := ctx.Err(); cause != nil {
			return probeResult{kind: probeCancelled, err: cause}
		}
		if channelClosed(exited) {
			return probeResult{kind: probeExited}
		}
		return result
	}
}

type observation struct {
	continuePolling bool
}

type healthPayload struct {
	Ready            json.RawMessage `json:"ready"`
	BackgroundStatus json.RawMessage `json:"backgroundStatus"`
	BackgroundError  json.RawMessage `json:"backgroundError"`
	Protocol         json.RawMessage `json:"protocol"`
	Version          json.RawMessage `json:"version"`
	Commit           json.RawMessage `json:"commit"`
}

func inspectResponse(httpStatus int, body []byte, bodyErr error, expected Expectation) (observation, error) {
	if httpStatus != http.StatusOK {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应状态不是 200", map[string]any{"status": httpStatus}, nil)
	}
	if bodyErr != nil {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应正文无效", nil, bodyErr)
	}
	payload, err := decodePayload(body)
	if err != nil {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应 JSON 无效", nil, err)
	}
	ready, ok := parseBool(payload.Ready)
	if !ok {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应的 ready 字段无效", map[string]any{"field": "ready"}, nil)
	}
	status, ok := parseString(payload.BackgroundStatus)
	if !ok {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应的 backgroundStatus 字段无效", map[string]any{"field": "backgroundStatus"}, nil)
	}
	backgroundError, ok := parseNullableString(payload.BackgroundError)
	if !ok {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端健康响应的 backgroundError 字段无效", map[string]any{"field": "backgroundError"}, nil)
	}
	if backgroundError != "" {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端报告了后台错误", map[string]any{"field": "backgroundError"}, nil)
	}
	if status == "failed" || status == "cancelled" {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端后台状态异常终止", map[string]any{"field": "backgroundStatus"}, nil)
	}
	if status != "starting" && status != "running" && status != "ready" {
		return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端后台状态未知", map[string]any{"field": "backgroundStatus"}, nil)
	}
	actualProtocol, protocolOK := parseInt(payload.Protocol)
	if !protocolOK {
		return observation{}, identityError(expected, "protocol", nil)
	}
	if actualProtocol != expected.Protocol {
		return observation{}, identityError(expected, "protocol", actualProtocol)
	}
	if expected.Mode == ModeManaged {
		if err := validateManagedIdentity(payload, expected); err != nil {
			return observation{}, err
		}
	}
	switch status {
	case "starting", "running":
		return observation{continuePolling: true}, nil
	case "ready":
		if !ready {
			return observation{continuePolling: true}, nil
		}
		return observation{}, nil
	}
	return observation{}, newError(protocol.CodeBackendHealthInvalid, "后端后台状态未知", map[string]any{"field": "backgroundStatus"}, nil)
}

type bodyReadResult struct {
	body []byte
	err  error
}

func readResponseBody(ctx context.Context, response *http.Response) ([]byte, error) {
	if response.Body == nil {
		return nil, errors.New("health response body is nil")
	}
	done := make(chan bodyReadResult, 1)
	go func() {
		body, err := io.ReadAll(io.LimitReader(response.Body, maxHealthBodyBytes+1))
		done <- bodyReadResult{body: body, err: err}
	}()
	select {
	case result := <-done:
		closeErr := response.Body.Close()
		if result.err != nil || closeErr != nil {
			return nil, errors.Join(result.err, closeErr)
		}
		if len(result.body) > maxHealthBodyBytes {
			return nil, errors.New("health response body exceeds 64 KiB")
		}
		return result.body, nil
	case <-ctx.Done():
		closeErr := response.Body.Close()
		result := <-done
		return nil, errors.Join(ctx.Err(), result.err, closeErr)
	}
}

func decodePayload(body []byte) (healthPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil {
		return healthPayload{}, err
	}
	delim, ok := start.(json.Delim)
	if !ok || delim != '{' {
		return healthPayload{}, errors.New("health response must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return healthPayload{}, keyErr
		}
		key, keyOK := keyToken.(string)
		if !keyOK {
			return healthPayload{}, errors.New("health response object key is invalid")
		}
		if _, duplicate := fields[key]; duplicate {
			return healthPayload{}, fmt.Errorf("health response field %q is duplicated", key)
		}
		var value json.RawMessage
		if valueErr := decoder.Decode(&value); valueErr != nil {
			return healthPayload{}, valueErr
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil {
		return healthPayload{}, err
	}
	endDelim, ok := end.(json.Delim)
	if !ok || endDelim != '}' {
		return healthPayload{}, errors.New("health response object is not closed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return healthPayload{}, errors.New("health response contains trailing JSON")
		}
		return healthPayload{}, err
	}
	return healthPayload{
		Ready:            fields["ready"],
		BackgroundStatus: fields["backgroundStatus"],
		BackgroundError:  fields["backgroundError"],
		Protocol:         fields["protocol"],
		Version:          fields["version"],
		Commit:           fields["commit"],
	}, nil
}

func parseBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func parseString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func parseNullableString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if len(raw) == 0 {
			return "", false
		}
		return "", true
	}
	return parseString(raw)
}

func parseInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func validateManagedIdentity(payload healthPayload, expected Expectation) error {
	version, ok := parseString(payload.Version)
	if !ok || version != expected.Version {
		return identityError(expected, "version", version)
	}
	commit, ok := parseString(payload.Commit)
	if !ok || !validCommit(commit) || commit != expected.Commit {
		return identityError(expected, "commit", commit)
	}
	return nil
}

func identityError(_ Expectation, field string, got any) error {
	details := map[string]any{"field": field}
	if field == "protocol" && got != nil {
		details["got"] = got
	}
	return newError(protocol.CodeBackendIdentityMismatch, fmt.Sprintf("后端健康身份字段 %s 不匹配", field), details, nil)
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateExpectation(expected Expectation) error {
	if expected.Mode != ModeManaged && expected.Mode != ModeDevelopment {
		return newError(protocol.CodeBackendHealthInvalid, "不支持当前健康检查模式", map[string]any{"mode": expected.Mode}, nil)
	}
	if expected.Protocol != protocol.Version {
		return identityError(expected, "protocol", expected.Protocol)
	}
	if expected.Mode == ModeManaged {
		if expected.Version == "" {
			return identityError(expected, "version", nil)
		}
		if !validCommit(expected.Commit) {
			return identityError(expected, "commit", expected.Commit)
		}
	}
	return nil
}

func channelClosed(channel <-chan struct{}) bool {
	if channel == nil {
		return false
	}
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func closeResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	// 超时分支已返回主错误，异步收口不能再改变分类。
	_ = response.Body.Close()
}

func newDefaultTransport() Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: nil}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return clone
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(duration time.Duration) Timer {
	return systemTimer{timer: time.NewTimer(duration)}
}

type systemTimer struct {
	timer *time.Timer
}

func (t systemTimer) C() <-chan time.Time { return t.timer.C }

func (t systemTimer) Stop() bool { return t.timer.Stop() }
