package uv

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// WithPythonRelay 启用经回环中继的 Python 分发包下载（增补 2 C17 第 3 条）。
func WithPythonRelay(factory RelayFactory) PythonOption {
	return func(options *pythonOptions) error {
		if options == nil || factory == nil {
			return errors.New("python relay factory is invalid")
		}
		options.relay = factory
		return nil
	}
}

// WithPythonIntel 注入测速器，用于登记 Python 分发包探针目标并把 Accept-Ranges 能力交给中继。
func WithPythonIntel(intel sourceIntel) PythonOption {
	return func(options *pythonOptions) error {
		if options == nil || intel == nil {
			return errors.New("python source intel is invalid")
		}
		options.intel = intel
		return nil
	}
}

// installWithRelay 经中继执行一次 uv python install。
//
// handled=false 表示中继不可用（离线策略、没有上游或启动失败），调用方退回既有的环境变量轮换；
// 中继一旦启动，结局就是最终结局，换源已在中继内逐文件发生。
func (s *PythonService) installWithRelay(
	ctx context.Context,
	request PythonRequest,
	spec PythonSpec,
	installArgs []string,
	options RunOptions,
) (result UVResult, summary relay.Summary, handled bool, returnErr error) {
	if s.relay == nil || request.MirrorPolicy.Offline() {
		return UVResult{}, relay.Summary{}, false, nil
	}
	if s.intel != nil {
		// 探针目标就是要下的分发包；取不到只影响 python Kind 的测速，不影响安装。
		if path, err := pythonDownloadPath(ctx, s.runner, options, spec.Version); err == nil {
			s.intel.SetTarget(mirror.KindPython, mirror.ProbeTarget{
				Kind:        mirror.KindPython,
				Path:        path,
				WindowBytes: mirror.DefaultProbeWindowBytes,
			})
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return UVResult{}, relay.Summary{}, true, err
		}
	}
	plan, err := s.network.buildPlan(ctx, request.MirrorPolicy, mirror.KindPython)
	if err != nil {
		return UVResult{}, relay.Summary{}, true, err
	}
	upstreams := pythonRelayUpstreams(plan, s.probeResults(mirror.KindPython))
	if len(upstreams) == 0 {
		return UVResult{}, relay.Summary{}, false, nil
	}
	session, err := s.relay(ctx, relay.Config{
		StagingDir: s.layout.RelayStagingDir(),
		Upstreams:  map[relay.Route][]relay.Upstream{relay.RoutePython: upstreams},
	}, relay.Deps{Progress: request.Progress})
	if err != nil {
		return UVResult{}, relay.Summary{}, false, nil
	}
	defer func() {
		// 中继收口失败不改变安装结局；uv 已退出，暂存目录归可丢弃缓存。
		_ = session.Close()
	}()
	relayOptions := cloneRunOptions(options)
	relayOptions.Environment[uvPythonInstallMirrorEnv] = session.BaseURL() + "/" + relay.RoutePython.String()
	relayOptions.Environment[uvHTTPTimeoutEnv] = relayUVHTTPTimeoutSeconds
	result, runErr := s.runner.Run(ctx, append([]string(nil), installArgs...), relayOptions)
	summary = session.Summary()
	if runErr == nil && result.ExitCode == 0 {
		return result, summary, true, nil
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return result, summary, true, runErr
	}
	if len(summary.Failures) > 0 {
		return result, summary, true, newError(
			protocol.CodeMirrorExhausted,
			protocol.StagePythonInstall,
			"所有镜像源均不可用",
			map[string]any{
				"sourceKind":    mirror.KindPython.String(),
				"pythonVersion": spec.Version.String(),
				"exitCode":      result.ExitCode,
				"relay":         RelaySummaryDetails(summary),
			},
			nonNilRunError(runErr),
		)
	}
	return result, summary, true, pythonError(
		protocol.CodePythonInstallFailed,
		protocol.StagePythonInstall,
		"Python 安装失败",
		map[string]any{"pythonVersion": spec.Version.String(), "exitCode": result.ExitCode},
		nonNilRunError(runErr),
	)
}

func (s *PythonService) probeResults(kind mirror.Kind) []mirror.ProbeResult {
	if s.intel == nil {
		return nil
	}
	return s.intel.Results(kind)
}

// pythonRelayUpstreams 把 Python 分发源顺序翻译成中继 python 路由的上游。
func pythonRelayUpstreams(plan mirror.Plan, probes []mirror.ProbeResult) []relay.Upstream {
	acceptRanges := make(map[string]bool, len(probes))
	for _, probe := range probes {
		acceptRanges[probe.Source.Key()] = probe.OK && probe.AcceptRanges
	}
	sources := plan.Sources()
	upstreams := make([]relay.Upstream, 0, len(sources))
	for _, source := range sources {
		upstreams = append(upstreams, relay.Upstream{
			Key:         source.Key(),
			Base:        source.BaseURL() + "/",
			AcceptRange: acceptRanges[source.Key()],
		})
	}
	return upstreams
}
