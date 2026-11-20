package telemetry

import (
	_ "embed"
	"encoding/json"
	"slices"
)

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
