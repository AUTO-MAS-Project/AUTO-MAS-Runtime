package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// bootstrapWithRelayProgress 跑一次 bootstrap，假环境服务把预设的中继进度交给请求方回调。
func bootstrapWithRelayProgress(t *testing.T, progress []relay.Progress, summary *relay.Summary) []parsedEvent {
	t.Helper()
	root := t.TempDir()
	log := &m5TestLog{}
	environment := &m5TestEnvironment{calls: &log.calls, relayProgress: progress, relaySummary: summary}
	workspace := &m5TestWorkspace{calls: &log.calls, emitStates: true}
	var stdout, stderr bytes.Buffer
	code := Execute(
		context.Background(),
		[]string{"--app-root", root, "--output", "ndjson", "bootstrap", "--version", "v5.4.0"},
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		WithCWD(root),
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
		WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) { return workspace, nil }),
		WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
			return &m5TestStateStore{calls: &log.calls}, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			return &m5TestCoordinator{calls: &log.calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return log, nil
		}),
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", code, protocol.ExitCodeSuccess, stderr.String())
	}
	return parseNDJSON(t, stdout.String())
}

func detailedProgressEvents(events []parsedEvent, stage protocol.Stage) []parsedEvent {
	var detailed []parsedEvent
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) && eventString(event, "stage") == string(stage) && event.object["item"] != nil {
			detailed = append(detailed, event)
		}
	}
	return detailed
}

// TestBootstrap_DependenciesSyncEmitsBytes 锁定增补 2 C18：中继进度映射为 dependencies.sync 与 python.install 的
// 真实字节 running（current/total/percent/item/source/bytesPerSecond），且 result.details 带 relay 摘要。
func TestBootstrap_DependenciesSyncEmitsBytes(t *testing.T) {
	events := bootstrapWithRelayProgress(t, []relay.Progress{
		{Route: relay.RoutePackages, Item: "ab/cd/opencv_python-4.12.0.88-cp37-abi3-win_amd64.whl", Source: "aliyun", Received: 10_000_000, Total: 143_023_892, BytesPerSecond: 3_200_000},
		{Route: relay.RoutePackages, Item: "ab/cd/numpy-2.3.3-cp312-cp312-win_amd64.whl", Source: "pypi", Received: 9_000_000, Total: 143_023_892, BytesPerSecond: 2_000_000},
	}, &relay.Summary{Files: 2, Bytes: 143_023_892, BySource: map[string]int64{"aliyun": 100_000_000, "pypi": 43_023_892}})

	detailed := detailedProgressEvents(events, protocol.StageDependenciesSync)
	if len(detailed) != 2 {
		t.Fatalf("detailed dependencies.sync events = %d, want 2", len(detailed))
	}
	first := detailed[0]
	if eventString(first, "item") != "opencv_python-4.12.0.88-cp37-abi3-win_amd64.whl" || eventString(first, "source") != "aliyun" {
		t.Fatalf("first event item/source = %q/%q", eventString(first, "item"), eventString(first, "source"))
	}
	if rate, _ := first.object["bytesPerSecond"].(float64); int64(rate) != 3_200_000 {
		t.Fatalf("bytesPerSecond = %v", first.object["bytesPerSecond"])
	}
	assertMeasuredProgress(t, events, protocol.StageDependenciesSync, 10_000_000, 143_023_892, float64(10_000_000)/float64(143_023_892)*100)
	// 第二条 Received 回落（分片重取），字节必须单调不降。
	if current, _ := detailed[1].object["current"].(float64); int64(current) != 10_000_000 {
		t.Fatalf("second event current = %v, want clamped to 10000000", detailed[1].object["current"])
	}
	// python.install 同样带细目（假环境对两段回放同一份进度）。
	if got := len(detailedProgressEvents(events, protocol.StagePythonInstall)); got != 2 {
		t.Fatalf("python.install detailed events = %d, want 2", got)
	}
	result := events[len(events)-1]
	details, _ := result.object["details"].(map[string]any)
	relayDetails, ok := details["relay"].(map[string]any)
	if !ok || relayDetails["files"].(float64) != 2 {
		t.Fatalf("result.details.relay = %#v", details["relay"])
	}
	if _, ok := details["pythonRelay"].(map[string]any); !ok {
		t.Fatalf("result.details.pythonRelay missing: %#v", details)
	}
	if _, exists := details["networkProbe"]; exists {
		t.Fatalf("networkProbe present without any probing: %#v", details["networkProbe"])
	}
}

// TestBootstrap_RelayProgressWithoutTotalOmitsNumbers 锁定总量未知时不带数值、只带细目。
func TestBootstrap_RelayProgressWithoutTotalOmitsNumbers(t *testing.T) {
	events := bootstrapWithRelayProgress(t, []relay.Progress{
		{Route: relay.RoutePython, Item: "cpython-3.12.13.tar.gz", Source: "astral", Received: 5, Total: 0, BytesPerSecond: 1},
	}, nil)
	detailed := detailedProgressEvents(events, protocol.StagePythonInstall)
	if len(detailed) != 1 {
		t.Fatalf("detailed python.install events = %d, want 1", len(detailed))
	}
	for _, key := range []string{"current", "total", "percent"} {
		if _, exists := detailed[0].object[key]; exists {
			t.Fatalf("progress without total carries %q: %#v", key, detailed[0].object)
		}
	}
	if eventString(detailed[0], "item") != "cpython-3.12.13.tar.gz" {
		t.Fatalf("item = %q", eventString(detailed[0], "item"))
	}
	result := events[len(events)-1]
	details, _ := result.object["details"].(map[string]any)
	if _, exists := details["relay"]; exists {
		t.Fatalf("relay details present without a summary: %#v", details["relay"])
	}
}

// TestProbeEvents_ShapeAndDetails 锁定 network.probe 事件形态与 result.details.networkProbe 的结构。
func TestProbeEvents_ShapeAndDetails(t *testing.T) {
	var stdout bytes.Buffer
	output, err := protocol.NewProcessOutput(nopFlusher{&stdout})
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "bootstrap", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	ranker, err := mirror.NewRanker(catalog,
		mirror.WithRankerReport(probeProgress(emitter)),
		mirror.WithRankerRanked(probeRanked(emitter)),
		mirror.WithRankerProbeFunc(func(_ context.Context, plan mirror.Plan, _ mirror.ProbeTarget, report func(mirror.ProbeResult)) ([]mirror.ProbeResult, error) {
			results := make([]mirror.ProbeResult, 0, len(plan.Sources()))
			for index, source := range plan.Sources() {
				result := mirror.ProbeResult{Source: source, OK: index != 0, BytesPerSecond: int64(1_000_000 * (index + 1)), Bytes: 1024, TTFB: 20 * time.Millisecond, AcceptRanges: true}
				if index == 0 {
					result.BytesPerSecond, result.Bytes = 0, 0
				}
				report(result)
				results = append(results, result)
			}
			return results, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRanker() error = %v", err)
	}
	ranker.SetTarget(mirror.KindGit, mirror.ProbeTarget{Kind: mirror.KindGit, Path: gitProbePath})
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if _, err := ranker.Plan(context.Background(), policy, mirror.KindGit); err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	events := parseNDJSON(t, stdout.String())
	var probeEvents []parsedEvent
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) && eventString(event, "stage") == string(protocol.StageNetworkProbe) {
			probeEvents = append(probeEvents, event)
		}
	}
	if len(probeEvents) != 3 {
		t.Fatalf("network.probe events = %d, want 2 running + 1 succeeded", len(probeEvents))
	}
	first := probeEvents[0]
	if eventString(first, "status") != "running" || eventString(first, "item") != "cnb" || eventString(first, "source") != "cnb" {
		t.Fatalf("first probe event = %#v", first.object)
	}
	if rate, _ := first.object["bytesPerSecond"].(float64); rate != 0 || !strings.Contains(eventString(first, "message"), "不可用") {
		t.Fatalf("failed source event = %#v", first.object)
	}
	if total, _ := first.object["total"].(float64); total != 2 {
		t.Fatalf("total = %v, want 2 sources", first.object["total"])
	}
	last := probeEvents[2]
	if eventString(last, "status") != "succeeded" || !strings.Contains(eventString(last, "message"), "github → cnb") {
		t.Fatalf("succeeded event = %#v", last.object)
	}
	details := networkProbeDetails(ranker)
	entries, ok := details["git"].([]map[string]any)
	if !ok || len(entries) != 2 || entries[0]["source"] != "github" || entries[0]["ok"] != true || entries[1]["ok"] != false {
		t.Fatalf("networkProbe details = %#v", details)
	}
	if networkProbeDetails(nil) != nil {
		t.Fatal("networkProbeDetails(nil) != nil")
	}
}

type nopFlusher struct{ *bytes.Buffer }

func (nopFlusher) Flush() error { return nil }
