package cli

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

type telemetryFactory func(telemetry.Config) telemetry.Recorder

type sessionTelemetryState struct {
	recorder        telemetry.Recorder
	command         string
	stage           protocol.Stage
	runtimeVersion  string
	operationErr    error
	panicked        bool
	panicFrames     []telemetry.StackFrame
	terminalWritten bool
	sessionStarted  bool
}

// WithTelemetryFactory 注入遥测观察器工厂，供会话测试隔离真实 SDK 和网络。
func WithTelemetryFactory(factory telemetryFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli telemetry factory must not be nil")
		}
		values.telemetryFactory = factory
		return nil
	}
}

func newTelemetryRecorder(deps *deps, runtimeVersion string) telemetry.Recorder {
	if deps == nil {
		return nil
	}
	config := telemetry.LoadConfig()
	config.Offline = deps.global.mirrorPolicy.Offline()
	if !config.Enabled || config.Offline {
		return nil
	}
	if config.SentryRelease == "" {
		config.SentryRelease = strings.TrimSpace(runtimeVersion)
	}
	factory := deps.options.telemetryFactory
	if factory == nil {
		factory = telemetry.New
	}
	return safeTelemetryFactory(factory, config)
}

func safeTelemetryFactory(factory telemetryFactory, config telemetry.Config) (recorder telemetry.Recorder) {
	defer func() {
		if recover() != nil {
			recorder = nil
		}
	}()
	return factory(config)
}

func recordSessionTelemetry(
	recorder telemetry.Recorder,
	command string,
	fallbackStage protocol.Stage,
	runtimeVersion string,
	operationErr error,
	panicked bool,
	panicFrames []telemetry.StackFrame,
) {
	if recorder == nil {
		return
	}
	if recoveredFrames, recovered := recoveredControlReaderPanic(operationErr); recovered {
		panicked = true
		if len(panicFrames) == 0 {
			panicFrames = recoveredFrames
		}
	}
	stage := fallbackStage
	internalError := false
	if operationErr != nil {
		code, classifiedStage, _, _ := classifyFailure(operationErr, fallbackStage)
		stage = classifiedStage
		internalError = code == protocol.CodeInternalError
	}
	if internalError || panicked {
		safeTelemetryCaptureInternal(recorder, telemetry.InternalObservation{
			Command:         command,
			Stage:           string(stage),
			Code:            string(protocol.CodeInternalError),
			RuntimeVersion:  runtimeVersion,
			ProtocolVersion: protocol.Version,
			Platform:        runtime.GOOS + "/" + runtime.GOARCH,
			Panic:           panicked,
			PanicFrames:     append([]telemetry.StackFrame(nil), panicFrames...),
		})
	}
	safeTelemetryClose(recorder)
}

func newSessionTelemetryState(
	deps *deps,
	command string,
	stage protocol.Stage,
	runtimeVersion string,
	panicked bool,
	panicFrames []telemetry.StackFrame,
) *sessionTelemetryState {
	return &sessionTelemetryState{
		recorder:       newTelemetryRecorder(deps, runtimeVersion),
		command:        command,
		stage:          stage,
		runtimeVersion: runtimeVersion,
		panicked:       panicked,
		panicFrames:    append([]telemetry.StackFrame(nil), panicFrames...),
	}
}

func (s *sessionTelemetryState) finish() {
	if s == nil {
		return
	}
	if s.terminalWritten || (!s.sessionStarted && s.panicked) {
		recordSessionTelemetry(
			s.recorder,
			s.command,
			s.stage,
			s.runtimeVersion,
			s.operationErr,
			s.panicked,
			s.panicFrames,
		)
		return
	}
	safeTelemetryClose(s.recorder)
}

func (s *sessionTelemetryState) addPanic(operationErr error, panicFrames []telemetry.StackFrame) {
	if s == nil {
		return
	}
	s.operationErr = operationErr
	s.panicked = true
	if len(panicFrames) > 0 {
		s.panicFrames = append([]telemetry.StackFrame(nil), panicFrames...)
	}
}

func unexpectedPanicError(stage protocol.Stage) error {
	return &commandError{
		code:    protocol.CodeInternalError,
		stage:   stage,
		message: "命令执行失败",
		details: map[string]any{},
		cause:   errors.New("unexpected panic"),
	}
}

func safeEmitPanicFailure(deps *deps, emitter *protocol.Emitter, stage protocol.Stage, err error) (exitCode int, terminalWritten bool) {
	exitCode = protocol.ExitCodePreconditionFailed
	defer func() {
		if recover() != nil {
			terminalWritten = false
		}
	}()
	return emitFailure(deps, emitter, stage, err)
}

func helloRuntimeVersionSafely(ctx context.Context, source versionSourceFunc) (runtimeVersion string, panicked bool, panicFrames []telemetry.StackFrame) {
	runtimeVersion = devRuntimeVersion
	defer func() {
		if recover() != nil {
			panicked = true
			panicFrames = capturePanicFrames()
		}
	}()
	runtimeVersion = helloRuntimeVersion(ctx, source)
	return runtimeVersion, false, nil
}

func telemetryOutputFailure(stage protocol.Stage) error {
	return &commandError{
		code:    protocol.CodeOutputWriteFailed,
		stage:   stage,
		message: "协议输出失败",
		details: map[string]any{},
		cause:   errors.New("final result output failed"),
	}
}

func safeTelemetryCaptureInternal(recorder telemetry.Recorder, observation telemetry.InternalObservation) {
	defer func() { _ = recover() }()
	if recorder != nil {
		recorder.CaptureInternal(context.Background(), observation)
	}
}

func safeTelemetryClose(recorder telemetry.Recorder) {
	defer func() { _ = recover() }()
	if recorder == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetry.DefaultFlushTimeout)
	defer cancel()
	recorder.Close(ctx)
}

func invokeSessionRun(
	stage protocol.Stage,
	run func() (sessionSuccess, error),
) (success sessionSuccess, err error, panicked bool, panicFrames []telemetry.StackFrame) {
	defer func() {
		if recover() != nil {
			panicked = true
			panicFrames = capturePanicFrames()
			success = sessionSuccess{}
			err = &commandError{
				code:    protocol.CodeInternalError,
				stage:   stage,
				message: "命令执行失败",
				details: map[string]any{},
				cause:   errors.New("unexpected panic"),
			}
		}
	}()
	success, err = run()
	if frames, ok := recoveredControlReaderPanic(err); ok {
		return success, err, true, frames
	}
	return success, err, false, nil
}

// controlReaderPanicError 只保留净化后的栈帧，避免把 panic 值或其原始文本带入错误流。
type controlReaderPanicError struct {
	frames []telemetry.StackFrame
}

func (e *controlReaderPanicError) Error() string {
	return "unexpected control reader panic"
}

// runControlReaderSafely 把控制读取 goroutine 中的 panic 转为可分类错误，避免直接终止进程。
func runControlReaderSafely(ctx context.Context, reader *protocol.ControlReader) (err error) {
	defer func() {
		if recover() != nil {
			err = &controlReaderPanicError{frames: capturePanicFrames()}
		}
	}()
	if reader == nil {
		return errors.New("control reader is nil")
	}
	return reader.Run(ctx)
}

func recoveredControlReaderPanic(err error) ([]telemetry.StackFrame, bool) {
	if err == nil {
		return nil, false
	}
	var panicErr *controlReaderPanicError
	if !errors.As(err, &panicErr) {
		return nil, false
	}
	return append([]telemetry.StackFrame(nil), panicErr.frames...), true
}

func capturePanicFrames() []telemetry.StackFrame {
	programCounters := make([]uintptr, 32)
	count := runtime.Callers(4, programCounters)
	if count == 0 {
		return nil
	}
	frames := runtime.CallersFrames(programCounters[:count])
	result := make([]telemetry.StackFrame, 0, count)
	for {
		frame, more := frames.Next()
		if frame.Function != "" {
			result = append(result, telemetry.StackFrame{
				Function: frame.Function,
				Lineno:   frame.Line,
			})
		}
		if !more || len(result) == cap(result) {
			break
		}
	}
	return result
}
