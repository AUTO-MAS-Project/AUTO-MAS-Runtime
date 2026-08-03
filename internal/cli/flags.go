package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// outputMode 是 --output 的稳定取值。
type outputMode string

const (
	outputHuman  outputMode = "human"
	outputNDJSON outputMode = "ndjson"
)

// outWriter 返回该输出模式下人类可读文本（帮助、human renderer）应写入的通道。
// ndjson 模式必须让出 stdout：那里只允许出现协议 NDJSON。
func (m outputMode) outWriter(streams IO) io.Writer {
	if m == outputNDJSON {
		return streams.Err
	}
	return streams.Out
}

// errProtocolMismatch 表示调用方协议版本与 Runtime 不兼容，退出码 10。
var errProtocolMismatch = errors.New("protocol version mismatch")

// globalOptions 保存解析成功后的全局参数值。
type globalOptions struct {
	layout          *config.Layout
	output          outputMode
	protocolVersion int
	mirrorPolicy    mirror.Policy
}

// validateOutputAndProtocol 只校验 --output 与 --protocol 的语义，供 Execute
// 在 help 分支之前调用：非法 output → 普通错误（退出码 2），协议不兼容 →
// errProtocolMismatch（退出码 10），保证 --help 不压过这两项校验。
func validateOutputAndProtocol(flags *pflag.FlagSet) (outputMode, error) {
	rawOutput, err := flags.GetString("output")
	if err != nil {
		return "", err
	}
	var mode outputMode
	switch rawOutput {
	case string(outputHuman):
		mode = outputHuman
	case string(outputNDJSON):
		mode = outputNDJSON
	default:
		return "", fmt.Errorf("invalid output mode %q, want human or ndjson", rawOutput)
	}
	rawProtocol, err := flags.GetString("protocol")
	if err != nil {
		return "", err
	}
	protocolVersion, err := strconv.Atoi(rawProtocol)
	if err != nil {
		return "", fmt.Errorf("invalid protocol %q, want integer", rawProtocol)
	}
	if protocolVersion != protocol.Version {
		return "", errProtocolMismatch
	}
	return mode, nil
}

// parseGlobalOptions 在 Cobra 完成 flag 解析后执行全局参数语义校验。
// output/protocol 复用 validateOutputAndProtocol，与 Execute 预解析阶段
// 保持同一份判定，不再各写一遍。
// 校验失败返回普通错误（退出码 2），协议不兼容返回 errProtocolMismatch（退出码 10）。
func parseGlobalOptions(flags *pflag.FlagSet, cwd string) (globalOptions, error) {
	mode, err := validateOutputAndProtocol(flags)
	if err != nil {
		return globalOptions{}, err
	}

	rawAppRoot, err := flags.GetString("app-root")
	if err != nil {
		return globalOptions{}, err
	}
	layout, err := config.NewLayout(rawAppRoot, cwd)
	if err != nil {
		return globalOptions{}, fmt.Errorf("invalid app root %q: %w", rawAppRoot, err)
	}

	preferred, err := parseMirrorPreferences(flags)
	if err != nil {
		return globalOptions{}, err
	}

	offline, err := flags.GetBool("offline")
	if err != nil {
		return globalOptions{}, err
	}
	mirrorOnly, err := flags.GetBool("mirror-only")
	if err != nil {
		return globalOptions{}, err
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{
		Preferred:  preferred,
		Offline:    offline,
		MirrorOnly: mirrorOnly,
	})
	if err != nil {
		return globalOptions{}, fmt.Errorf("invalid mirror policy: %w", err)
	}

	return globalOptions{
		layout:          layout,
		output:          mode,
		protocolVersion: protocol.Version,
		mirrorPolicy:    policy,
	}, nil
}

// parseMirrorPreferences 解析可重复的 --mirror <kind>=<key>，同一 kind 只允许一次。
func parseMirrorPreferences(flags *pflag.FlagSet) (map[mirror.Kind]string, error) {
	rawMirrors, err := flags.GetStringArray("mirror")
	if err != nil {
		return nil, err
	}
	preferred := make(map[mirror.Kind]string, len(rawMirrors))
	for _, raw := range rawMirrors {
		kindText, key, found := strings.Cut(raw, "=")
		if !found || kindText == "" || key == "" {
			return nil, fmt.Errorf("invalid mirror %q, want <kind>=<key>", raw)
		}
		kind := mirror.Kind(kindText)
		if _, seen := preferred[kind]; seen {
			return nil, fmt.Errorf("duplicate mirror kind %q", kindText)
		}
		preferred[kind] = key
	}
	return preferred, nil
}
