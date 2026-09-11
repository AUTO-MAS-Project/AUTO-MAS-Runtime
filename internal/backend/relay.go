package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// RelaySession 是监督器消费的最小中继能力；生产实现是 *relay.Server。
type RelaySession interface {
	BaseURL() string
	SetUpstreams(route relay.Route, upstreams []relay.Upstream) error
	Close() error
}

// RelayStarter 启动一次受监督期间常驻的中继会话（增补 2 C17 第 8 条）。
type RelayStarter func(ctx context.Context, cfg relay.Config, deps relay.Deps) (RelaySession, error)

// productionRelayStarter 包装 relay.Start。
func productionRelayStarter(ctx context.Context, cfg relay.Config, deps relay.Deps) (RelaySession, error) {
	return relay.Start(ctx, cfg, deps)
}

// relayDiagnostics 把中继与后台测速的诊断送进监督日志。
//
// 中继在后端 Logger 建立之前就要起来（spawn 之前），因此诊断先缓冲，Logger 一旦建立（每次监督尝试各建一个）
// 就 attach 过来并冲刷；缓冲有上限，超出丢最旧的一条。
type relayDiagnostics struct {
	mu      sync.Mutex // 保护 logger 与 pending
	logger  Logger
	pending []string
}

const relayDiagnosticsBacklog = 64

func (d *relayDiagnostics) record(ctx context.Context, message string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	logger := d.logger
	if logger == nil {
		if len(d.pending) >= relayDiagnosticsBacklog {
			d.pending = d.pending[1:]
		}
		d.pending = append(d.pending, message)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	_ = logger.Record(ctx, process.StreamRecord{Stream: "relay", Fragment: message, EndOfLine: true})
}

// attach 绑定当前监督尝试的 Logger 并冲刷缓冲；nil 表示解绑（尝试结束、Logger 已关闭）。
func (d *relayDiagnostics) attach(ctx context.Context, logger Logger) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.logger = logger
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	if logger == nil {
		return
	}
	for _, message := range pending {
		_ = logger.Record(ctx, process.StreamRecord{Stream: "relay", Fragment: message, EndOfLine: true})
	}
}

// supervisedRelay 是一次监督期间的中继与后台测速状态。
type supervisedRelay struct {
	session RelaySession
	wait    sync.WaitGroup
}

// startSupervisedRelay 按目录顺序起中继、注入回环首项，并在后台按实测结果替换上游顺序。
//
// 后端启动不等测速：中继先按目录顺序服务，探测（每类 3 秒预算）完成后再换成实测顺序。
// 中继起不来只降级为今天的直连列表，不影响监督；返回的 infrastructure 已含回环首项。
func (s *ManagedSupervisor) startSupervisedRelay(
	ctx context.Context,
	diagnostics *relayDiagnostics,
) (*supervisedRelay, uv.SupervisionInfrastructure, error) {
	infrastructure, err := supervisionInfrastructureWithPlan(ctx, s.layout, s.deps.MirrorPolicy, s.planFunc())
	if err != nil {
		return nil, uv.SupervisionInfrastructure{}, err
	}
	if s.deps.Relay == nil || s.deps.MirrorPolicy.Offline() {
		return nil, infrastructure, nil
	}
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return nil, infrastructure, nil
	}
	plans, err := supervisedRelayPlans(ctx, catalog, s.deps.MirrorPolicy)
	if err != nil {
		return nil, infrastructure, nil
	}
	upstreams := relayUpstreamsFromPlans(plans, nil)
	session, err := s.deps.Relay(ctx, relay.Config{
		StagingDir: s.layout.RelayStagingDir(),
		Upstreams:  upstreams,
	}, relay.Deps{Logger: relayLogger(diagnostics)})
	if err != nil {
		diagnostics.record(ctx, "relay start failed, backend keeps direct mirror lists: "+err.Error())
		return nil, infrastructure, nil
	}
	diagnostics.record(ctx, "relay listening at "+session.BaseURL())
	infrastructure.RelayBaseURL = session.BaseURL()
	managed := &supervisedRelay{session: session}
	if ranker := s.deps.Ranker; ranker != nil {
		managed.wait.Add(1)
		go func() {
			defer managed.wait.Done()
			s.rankSupervisedRelay(ctx, ranker, session, diagnostics)
		}()
	}
	return managed, infrastructure, nil
}

// rankSupervisedRelay 在后台探测包索引源并把实测顺序交给中继；Python 源没有可靠的探针目标，保持目录顺序。
func (s *ManagedSupervisor) rankSupervisedRelay(
	ctx context.Context,
	ranker *mirror.Ranker,
	session RelaySession,
	diagnostics *relayDiagnostics,
) {
	if target, ok := packageIndexProbeTarget(s.layout.UVLockFile(), s.layout.PythonVersionFile()); ok {
		ranker.SetTarget(mirror.KindPackageIndex, target)
	}
	plan, err := buildPlanOrDefault(ctx, ranker.PlanFunc(), s.deps.MirrorPolicy, mirror.KindPackageIndex)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			diagnostics.record(ctx, "relay ranking failed, keeping catalog order: "+err.Error())
		}
		return
	}
	upstreams := relayUpstreamsFromPlans(map[mirror.Kind]mirror.Plan{mirror.KindPackageIndex: plan}, ranker.Results(mirror.KindPackageIndex))
	for _, route := range []relay.Route{relay.RoutePackages, relay.RouteSimple} {
		if list := upstreams[route]; len(list) > 0 {
			if err := session.SetUpstreams(route, list); err != nil {
				diagnostics.record(ctx, fmt.Sprintf("relay upstream update rejected for %s: %v", route, err))
				continue
			}
			keys := make([]string, 0, len(list))
			for _, upstream := range list {
				keys = append(keys, upstream.Key)
			}
			diagnostics.record(ctx, fmt.Sprintf("relay %s upstreams reordered by probe: %v", route, keys))
		}
	}
}

// close 等待后台测速退出并关闭中继；nil 接收者安全。
func (r *supervisedRelay) close() error {
	if r == nil {
		return nil
	}
	r.wait.Wait()
	return r.session.Close()
}

// packageIndexProbeTarget 用受管仓库锁内最大制品作为包索引探针；锁不可读或规划失败时不探测。
func packageIndexProbeTarget(lockPath, versionPath string) (mirror.ProbeTarget, bool) {
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return mirror.ProbeTarget{}, false
	}
	version := uv.PythonVersion{Major: 3, Minor: 12}
	if raw, err := os.ReadFile(versionPath); err == nil {
		if parsed, ok := uv.ParsePythonVersion(string(raw)); ok {
			version = parsed
		}
	}
	plan, err := uv.PlanLock(string(lock), version)
	if err != nil || plan.Largest.Path == "" {
		return mirror.ProbeTarget{}, false
	}
	return mirror.ProbeTarget{Kind: mirror.KindPackageIndex, Path: plan.Largest.Path, WindowBytes: mirror.DefaultProbeWindowBytes}, true
}

// supervisedRelayPlans 按目录顺序为中继需要的两类源构造 Plan（不探测）。
func supervisedRelayPlans(ctx context.Context, catalog *mirror.Catalog, policy mirror.Policy) (map[mirror.Kind]mirror.Plan, error) {
	plans := make(map[mirror.Kind]mirror.Plan, 2)
	plan := mirror.CatalogPlanFunc(catalog)
	for _, kind := range []mirror.Kind{mirror.KindPackageIndex, mirror.KindPython} {
		built, err := buildPlanOrDefault(ctx, plan, policy, kind)
		if err != nil {
			return nil, err
		}
		plans[kind] = built
	}
	return plans, nil
}

// relayUpstreamsFromPlans 把包索引与 Python 源顺序翻译成中继三条路由的上游。
func relayUpstreamsFromPlans(plans map[mirror.Kind]mirror.Plan, probes []mirror.ProbeResult) map[relay.Route][]relay.Upstream {
	acceptRanges := make(map[string]bool, len(probes))
	for _, probe := range probes {
		acceptRanges[probe.Source.Key()] = probe.OK && probe.AcceptRanges
	}
	upstreams := make(map[relay.Route][]relay.Upstream, 3)
	if plan, ok := plans[mirror.KindPackageIndex]; ok {
		for _, source := range plan.Sources() {
			rewrite, ok := source.PackageIndexRewrite()
			if !ok {
				continue
			}
			upstreams[relay.RoutePackages] = append(upstreams[relay.RoutePackages], relay.Upstream{
				Key: source.Key(), Base: rewrite.PackagesBase(), AcceptRange: acceptRanges[source.Key()],
			})
			upstreams[relay.RouteSimple] = append(upstreams[relay.RouteSimple], relay.Upstream{
				Key: source.Key(), Base: rewrite.SimpleBase() + "/", AcceptRange: acceptRanges[source.Key()], RewriteFrom: rewrite.PackagesBase(),
			})
		}
	}
	if plan, ok := plans[mirror.KindPython]; ok {
		for _, source := range plan.Sources() {
			upstreams[relay.RoutePython] = append(upstreams[relay.RoutePython], relay.Upstream{Key: source.Key(), Base: source.BaseURL() + "/"})
		}
	}
	return upstreams
}

// relayLogger 把中继引擎的日志转成监督日志里的 relay 流记录（经缓冲 sink）。
func relayLogger(diagnostics *relayDiagnostics) func(level, message string, fields map[string]any) {
	if diagnostics == nil {
		return nil
	}
	return func(level, message string, fields map[string]any) {
		diagnostics.record(context.Background(), fmt.Sprintf("[%s] %s %v", level, message, fields))
	}
}

// planFunc 返回监督器使用的尝试顺序来源：有测速器用它，否则目录顺序。
func (s *ManagedSupervisor) planFunc() mirror.PlanFunc {
	if s.deps.Ranker != nil {
		return s.deps.Ranker.PlanFunc()
	}
	return nil
}
