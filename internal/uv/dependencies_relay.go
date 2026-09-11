package uv

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// relaySourceKey 是中继路径在 MirrorAttempt / details 里的源标识；真实来源由 relay.Summary.BySource 给出。
const relaySourceKey = "relay"

// relaySession 是依赖同步与 Python 安装消费的最小中继能力；生产实现是 *relay.Server。
type relaySession interface {
	BaseURL() string
	Register(items []relay.Item)
	Summary() relay.Summary
	Close() error
}

// RelayFactory 启动一次中继会话；生产实现包装 relay.Start，测试注入替身。
type RelayFactory func(ctx context.Context, cfg relay.Config, deps relay.Deps) (relaySession, error)

// ProductionRelayFactory 返回包装 relay.Start 的工厂。
func ProductionRelayFactory() RelayFactory {
	return func(ctx context.Context, cfg relay.Config, deps relay.Deps) (relaySession, error) {
		return relay.Start(ctx, cfg, deps)
	}
}

// sourceIntel 是各服务从测速器读取的最小能力：登记探针目标、读取实测结果（Accept-Ranges 等）。
// 生产实现是 *mirror.Ranker；不注入时探针目标无处登记，中继上游的 Range 能力由中继自行探知。
type sourceIntel interface {
	SetTarget(kind mirror.Kind, target mirror.ProbeTarget)
	Results(kind mirror.Kind) []mirror.ProbeResult
}

// WithDependenciesRelay 启用经回环中继的依赖同步（增补 2 C17）；nil 工厂视为参数错误。
func WithDependenciesRelay(factory RelayFactory) DependenciesOption {
	return func(options *dependenciesOptions) error {
		if options == nil || factory == nil {
			return errors.New("dependencies relay factory is invalid")
		}
		options.relay = factory
		return nil
	}
}

// WithDependenciesIntel 注入测速器，用于登记包索引探针目标并把 Accept-Ranges 能力交给中继。
func WithDependenciesIntel(intel sourceIntel) DependenciesOption {
	return func(options *dependenciesOptions) error {
		if options == nil || intel == nil {
			return errors.New("dependencies source intel is invalid")
		}
		options.intel = intel
		return nil
	}
}

// syncWithRelay 执行一次经中继的 uv sync --frozen。
//
// 返回 handled=false 表示中继没能启动，调用方退回 T13.4 的逐源轮换；一旦中继启动，结局就是最终结局：
// 换源已在中继内逐文件发生，再对外层做直连回退只会把同样的失败重来一遍。
func (s *DependenciesService) syncWithRelay(
	ctx context.Context,
	request DependenciesRequest,
	lock string,
	projectFile string,
) (result DependenciesResult, handled bool, returnErr error) {
	if s.relay == nil {
		return DependenciesResult{}, false, nil
	}
	plan, planErr := PlanLock(lock, parsePythonVersionLoose(request.PythonVersion))
	if planErr == nil && s.intel != nil && plan.Largest.Path != "" {
		s.intel.SetTarget(mirror.KindPackageIndex, mirror.ProbeTarget{
			Kind:        mirror.KindPackageIndex,
			Path:        plan.Largest.Path,
			WindowBytes: mirror.DefaultProbeWindowBytes,
		})
	}
	sourcePlan, err := s.buildPackageIndexPlan(ctx, request.MirrorPolicy)
	if err != nil {
		return DependenciesResult{}, true, err
	}
	upstreams := relayUpstreams(sourcePlan, s.results(mirror.KindPackageIndex))
	if len(upstreams[relay.RoutePackages]) == 0 {
		return DependenciesResult{}, false, nil
	}
	items := make([]relay.Item, 0, len(plan.Items))
	for _, artifact := range plan.Items {
		items = append(items, relay.Item{Path: artifact.Path, Size: artifact.Size, SHA256: artifact.SHA256})
	}
	session, err := s.relay(ctx, relay.Config{
		StagingDir: s.layout.RelayStagingDir(),
		Upstreams:  upstreams,
		Items:      items,
	}, relay.Deps{Progress: request.Progress})
	if err != nil {
		return DependenciesResult{}, false, nil
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil && returnErr == nil {
			// 中继收口失败不改变同步结局；uv 已经退出，暂存目录归可丢弃缓存。
			_ = closeErr
		}
	}()
	rewrite, err := mirror.NewLoopbackRewrite(session.BaseURL())
	if err != nil {
		return DependenciesResult{}, true, fmt.Errorf("build relay rewrite: %w", err)
	}
	if err := notifyMirrorAttempt(ctx, request.Attempt, MirrorAttempt{
		SourceKind: mirror.KindPackageIndex.String(),
		Source:     relaySourceKey,
		SourceTry:  1,
		GlobalTry:  1,
	}); err != nil {
		return DependenciesResult{}, true, err
	}
	relayRequest := request
	relayRequest.Line = request.Line
	uvResult, runErr := s.runStagedSyncWithEnvironment(ctx, relayRequest, rewrite, lock, projectFile, map[string]string{
		uvHTTPTimeoutEnv: relayUVHTTPTimeoutSeconds,
	})
	summary := session.Summary()
	if runErr == nil && uvResult.ExitCode == 0 {
		return DependenciesResult{
			LockfileChecked: true,
			Synchronized:    true,
			SourceKind:      mirror.KindPackageIndex.String(),
			Source:          dominantSource(summary),
			AttemptCount:    1,
			LockRewritten:   true,
			Relay:           &summary,
		}, true, nil
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return DependenciesResult{}, true, runErr
	}
	if len(summary.Failures) > 0 {
		return DependenciesResult{}, true, newError(
			protocol.CodeMirrorExhausted,
			protocol.StageDependenciesSync,
			"所有镜像源均不可用",
			map[string]any{
				"sourceKind":   mirror.KindPackageIndex.String(),
				"exitCode":     uvResult.ExitCode,
				"attemptCount": 1,
				"relay":        RelaySummaryDetails(summary),
			},
			nonNilRunError(runErr),
		)
	}
	return DependenciesResult{}, true, dependencySyncError(uvResult, runErr)
}

// results 返回测速器记录的探测结果；没有测速器时为 nil。
func (s *DependenciesService) results(kind mirror.Kind) []mirror.ProbeResult {
	if s.intel == nil {
		return nil
	}
	return s.intel.Results(kind)
}

// relayUpstreams 把包索引源顺序翻译成中继的 packages 与 simple 两条路由上游（增补 2 C17 第 1 条）。
func relayUpstreams(plan mirror.Plan, probes []mirror.ProbeResult) map[relay.Route][]relay.Upstream {
	acceptRanges := make(map[string]bool, len(probes))
	for _, probe := range probes {
		acceptRanges[probe.Source.Key()] = probe.OK && probe.AcceptRanges
	}
	sources := plan.Sources()
	packages := make([]relay.Upstream, 0, len(sources))
	simple := make([]relay.Upstream, 0, len(sources))
	for _, source := range sources {
		rewrite, ok := source.PackageIndexRewrite()
		if !ok {
			continue
		}
		packages = append(packages, relay.Upstream{
			Key:         source.Key(),
			Base:        rewrite.PackagesBase(),
			AcceptRange: acceptRanges[source.Key()],
		})
		simple = append(simple, relay.Upstream{
			Key:         source.Key(),
			Base:        rewrite.SimpleBase() + "/",
			AcceptRange: acceptRanges[source.Key()],
			RewriteFrom: rewrite.PackagesBase(),
		})
	}
	return map[relay.Route][]relay.Upstream{
		relay.RoutePackages: packages,
		relay.RouteSimple:   simple,
	}
}

// dominantSource 返回贡献字节最多的源 key；没有字节记录时返回 relay。
func dominantSource(summary relay.Summary) string {
	best, bestBytes := relaySourceKey, int64(-1)
	keys := make([]string, 0, len(summary.BySource))
	for key := range summary.BySource {
		keys = append(keys, key)
	}
	// 固定遍历顺序，让并列时的结果可复现。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for _, key := range keys {
		if bytes := summary.BySource[key]; bytes > bestBytes {
			best, bestBytes = key, bytes
		}
	}
	return best
}

// RelaySummaryDetails 把中继摘要整理成 result.details.relay（增补 2 C18 第 7 条）。
func RelaySummaryDetails(summary relay.Summary) map[string]any {
	bySource := make(map[string]any, len(summary.BySource))
	for key, bytes := range summary.BySource {
		bySource[key] = bytes
	}
	failures := make([]map[string]any, 0, len(summary.Failures))
	for _, failure := range summary.Failures {
		attempts := make([]map[string]any, 0, len(failure.Attempts))
		for _, attempt := range failure.Attempts {
			attempts = append(attempts, map[string]any{"source": attempt.Source, "outcome": attempt.Outcome.String()})
		}
		failures = append(failures, map[string]any{"item": filepath.Base(failure.Item), "attempts": attempts})
	}
	return map[string]any{
		"files":    summary.Files,
		"bytes":    summary.Bytes,
		"bySource": bySource,
		"failures": failures,
	}
}

// parsePythonVersionLoose 把 "3.12.13" 形式的版本转成 PythonVersion；解析不了时返回零值（规划器仍能工作，
// 只是 cp312 一类的匹配退化为 none-any 优先，方向仍是高估）。
func parsePythonVersionLoose(value string) PythonVersion {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) < 2 {
		return PythonVersion{}
	}
	var version PythonVersion
	fields := []*int{&version.Major, &version.Minor, &version.Patch}
	for index, part := range parts {
		if index >= len(fields) {
			break
		}
		number := 0
		for _, character := range part {
			if character < '0' || character > '9' {
				return PythonVersion{}
			}
			number = number*10 + int(character-'0')
		}
		*fields[index] = number
	}
	return version
}
