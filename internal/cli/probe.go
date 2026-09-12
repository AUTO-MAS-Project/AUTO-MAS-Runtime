package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// gitProbePath 是 Git 源的探针路径：smart HTTP 的引用公告，只看 TTFB（增补 2 C16）。
const gitProbePath = "info/refs?service=git-upload-pack"

// newNetworkRanker 为本次操作创建按实测排序的 PlanFunc 提供者，并把 uv 与 git 两类静态探针目标登记好。
//
// 每个源探测完成发一条 network.probe running，一轮排序完成发一条 succeeded；探测本身失败只落操作日志，
// 不会让操作失败（C16 第 4 条）。返回的 Ranker 只在本进程内有效，缓存也随进程结束。
func newNetworkRanker(emitter *protocol.Emitter, opLog *workspaceLogBinding) (*mirror.Ranker, error) {
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return nil, fmt.Errorf("build network catalog: %w", err)
	}
	options := []mirror.RankerOption{
		mirror.WithRankerLogger(func(message string) {
			if opLog == nil {
				return
			}
			logger := opLog.Get()
			if logger == nil {
				return
			}
			// 探测失败是可容忍的降级，只记 warning；写日志失败不影响排序结果。
			_, _ = logger.Record(context.Background(), logging.LevelWarn, message, map[string]any{})
		}),
	}
	if emitter != nil {
		options = append(options,
			mirror.WithRankerReport(probeProgress(emitter)),
			mirror.WithRankerRanked(probeRanked(emitter)),
		)
	}
	ranker, err := mirror.NewRanker(catalog, options...)
	if err != nil {
		return nil, err
	}
	ranker.SetTarget(mirror.KindUV, mirror.ProbeTarget{
		Kind:        mirror.KindUV,
		Path:        uv.FixedVersion + "/" + uv.WindowsX64Artifact,
		WindowBytes: mirror.DefaultProbeWindowBytes,
	})
	ranker.SetTarget(mirror.KindGit, mirror.ProbeTarget{Kind: mirror.KindGit, Path: gitProbePath})
	return ranker, nil
}

// probeProgress 把单个源的探测结果报成 network.probe running：item 与 source 都是源 key，
// current/total 是已完成源数与源总数。bytesPerSecond 三态：正数为实测吞吐，0 为探测失败，
// **缺失**表示探测成功但只测了首字节（git 类源没有吞吐可报）——调用方不得把缺失当失败。
func probeProgress(emitter *protocol.Emitter) mirror.ProbeReportFunc {
	return func(kind mirror.Kind, result mirror.ProbeResult, completed, total int) error {
		current, totalCount := int64(completed), int64(total)
		key := result.Source.Key()
		message := fmt.Sprintf("测速 %s（%s）：%s", key, kind, formatProbeRate(result))
		event := protocol.ProgressEvent{
			Stage:   protocol.StageNetworkProbe,
			Status:  protocol.ProgressRunning,
			Current: &current,
			Total:   &totalCount,
			Item:    key,
			Source:  key,
			Message: message,
		}
		if !result.OK || result.Bytes > 0 {
			rate := result.BytesPerSecond
			event.BytesPerSecond = &rate
		}
		if err := emitter.EmitProgress(event); err != nil {
			return &commandError{
				code:    protocol.CodeOutputWriteFailed,
				stage:   protocol.StageNetworkProbe,
				message: "协议输出失败",
				details: map[string]any{},
				cause:   err,
			}
		}
		return nil
	}
}

// probeRanked 在一轮排序完成后发 network.probe succeeded，message 给出该 Kind 的最终顺序。
func probeRanked(emitter *protocol.Emitter) mirror.RankedFunc {
	return func(kind mirror.Kind, ranked mirror.Plan, _ []mirror.ProbeResult) error {
		keys := make([]string, 0, len(ranked.Sources()))
		for _, source := range ranked.Sources() {
			keys = append(keys, source.Key())
		}
		if err := emitter.EmitProgress(protocol.ProgressEvent{
			Stage:   protocol.StageNetworkProbe,
			Status:  protocol.ProgressSucceeded,
			Message: fmt.Sprintf("%s 源顺序：%s", kind, strings.Join(keys, " → ")),
		}); err != nil {
			return &commandError{
				code:    protocol.CodeOutputWriteFailed,
				stage:   protocol.StageNetworkProbe,
				message: "协议输出失败",
				details: map[string]any{},
				cause:   err,
			}
		}
		return nil
	}
}

func formatProbeRate(result mirror.ProbeResult) string {
	if !result.OK {
		return "不可用"
	}
	if result.Bytes == 0 {
		return fmt.Sprintf("首字节 %d ms", result.TTFB.Milliseconds())
	}
	return fmt.Sprintf("%.2f MB/s", float64(result.BytesPerSecond)/(1024*1024))
}

// networkProbeDetails 把各 Kind 的实测结果整理成 result.details.networkProbe（增补 2 C18 第 7 条）。
// 没有探测过任何 Kind 时返回 nil，调用方据此决定是否写入 details。
func networkProbeDetails(ranker *mirror.Ranker) map[string]any {
	if ranker == nil {
		return nil
	}
	details := make(map[string]any, len(mirror.AllKinds()))
	for _, kind := range mirror.AllKinds() {
		results := ranker.Results(kind)
		if results == nil {
			continue
		}
		entries := make([]map[string]any, 0, len(results))
		for _, result := range results {
			entries = append(entries, map[string]any{
				"source":         result.Source.Key(),
				"ok":             result.OK,
				"ttfbMs":         result.TTFB.Milliseconds(),
				"bytesPerSecond": result.BytesPerSecond,
				"acceptRanges":   result.AcceptRanges,
			})
		}
		details[kind.String()] = entries
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

// rankerHolder 让生产工厂在会话建立后拿到本次操作的测速器；零值表示尚未建立，工厂退回目录顺序。
type rankerHolder struct {
	mu     sync.Mutex // 保护 ranker
	ranker *mirror.Ranker
}

func (h *rankerHolder) set(ranker *mirror.Ranker) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.ranker = ranker
	h.mu.Unlock()
}

func (h *rankerHolder) get() *mirror.Ranker {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ranker
}

// planFunc 返回测速器的 PlanFunc；没有测速器时返回 nil，调用方按目录顺序。
func (h *rankerHolder) planFunc() mirror.PlanFunc {
	ranker := h.get()
	if ranker == nil {
		return nil
	}
	return ranker.PlanFunc()
}

// withNetworkProbeDetails 把测速结果并入 result.details，不覆盖命令自己写的同名键。
func withNetworkProbeDetails(details map[string]any, probe map[string]any) map[string]any {
	if details == nil {
		details = map[string]any{}
	}
	if _, exists := details["networkProbe"]; !exists {
		details["networkProbe"] = probe
	}
	return details
}
