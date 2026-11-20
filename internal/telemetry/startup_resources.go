package telemetry

import (
	_ "embed"
	"encoding/json"
	"os"
	"slices"
)

// StartupSampleRequested 指示桌面请求本地阶段耗时；local 模式不会提供上报管道。
func StartupSampleRequested() bool {
	return os.Getenv("AUTO_MAS_STARTUP_SAMPLE") == "local" || StartupSampleInvitation() != ""
}

//go:embed startup_fields.json
var startupFieldsJSON []byte

var startupFields = func() struct {
	Values map[string][]string
	Fields map[string]string
} {
	var schema struct {
		Values map[string][]string
		Fields map[string]string
	}
	if err := json.Unmarshal(startupFieldsJSON, &schema); err != nil {
		panic(err)
	}
	return schema
}()

// sanitizeStartupResources 仅接收已分档的有限值，过滤发生在错误发送出口。
func sanitizeStartupResources(input map[string]any) map[string]any {
	age, _ := input["sample_age"].(string)
	if input["coverage"] != "sampled" || (age != "<1s" && age != "1-5s" && age != "5-15s") {
		return nil
	}
	clean := make(map[string]any)
	for key, raw := range input {
		allowed, known := startupFields.Fields[key]
		value, ok := raw.(string)
		if known && ok && (value == "unknown" || slices.Contains(startupFields.Values[allowed], value)) {
			clean[key] = value
		}
	}
	return clean
}
