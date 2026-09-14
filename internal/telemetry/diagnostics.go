package telemetry

import (
	"encoding"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	maxDiagnosticDepth       = 4
	maxDiagnosticFields      = 64
	maxDiagnosticSliceItems  = 32
	maxDiagnosticStringBytes = 512
	maxDiagnosticJSONBytes   = 8 * 1024
)

type diagnosticFieldKind uint8

const (
	diagnosticStableToken diagnosticFieldKind = iota + 1
	diagnosticIdentifier
	diagnosticSource
	diagnosticStableValue
	diagnosticPath
	diagnosticNumber
	diagnosticBoolean
	diagnosticContainer
	diagnosticDynamicNumberMap
	diagnosticProtocolCode
	diagnosticSHA256
)

// diagnosticFieldPolicies 是允许离开 Runtime 的结构字段白名单；未知字段默认丢弃。
var diagnosticFieldPolicies = map[string]diagnosticFieldKind{
	"operation": diagnosticStableToken, "component": diagnosticStableToken, "phase": diagnosticStableToken,
	"failureKind": diagnosticStableToken, "outcome": diagnosticStableToken, "field": diagnosticStableToken,
	"reason": diagnosticStableToken, "sourceKind": diagnosticStableToken, "source": diagnosticSource,
	"id": diagnosticStableToken, "status": diagnosticStableValue, "brokenStage": diagnosticStableToken,
	"mode": diagnosticStableToken, "state": diagnosticStableToken, "sink": diagnosticStableToken,
	"code":          diagnosticProtocolCode,
	"pythonVersion": diagnosticIdentifier, "expectedVersion": diagnosticIdentifier,
	"actualVersion": diagnosticIdentifier, "branch": diagnosticIdentifier, "commit": diagnosticIdentifier,
	"version": diagnosticIdentifier, "expectedProtocol": diagnosticStableValue, "receivedProtocol": diagnosticStableValue,
	"expectedSHA256": diagnosticSHA256,

	"path": diagnosticPath, "directory": diagnosticPath, "cwd": diagnosticPath, "log": diagnosticPath,
	"logPath": diagnosticPath, "previousLogPath": diagnosticPath, "projectDir": diagnosticPath,
	"projectEnvDir": diagnosticPath, "pythonInstallDir": diagnosticPath, "cacheDir": diagnosticPath,
	"filename": diagnosticPath, "item": diagnosticPath,

	"exitCode": diagnosticNumber, "previousExitCode": diagnosticNumber, "durationMs": diagnosticNumber,
	"windowsError": diagnosticNumber, "attemptCount": diagnosticNumber, "sourceTry": diagnosticNumber,
	"globalTry": diagnosticNumber, "capturedStdoutBytes": diagnosticNumber, "capturedStderrBytes": diagnosticNumber,
	"pid": diagnosticNumber, "previousPid": diagnosticNumber, "port": diagnosticNumber,
	"consecutiveErrors": diagnosticNumber, "got": diagnosticNumber,
	"total": diagnosticNumber, "cleaned": diagnosticNumber, "skipped": diagnosticNumber,
	"failed": diagnosticNumber, "files": diagnosticNumber, "bytes": diagnosticNumber,
	"maxBytes": diagnosticNumber, "count": diagnosticNumber, "orphanCount": diagnosticNumber,

	"partial": diagnosticBoolean, "committed": diagnosticBoolean, "removed": diagnosticBoolean,
	"truncated": diagnosticBoolean, "lockRewritten": diagnosticBoolean, "synchronized": diagnosticBoolean,
	"lockfileChecked": diagnosticBoolean, "orphansTruncated": diagnosticBoolean,

	"attempts": diagnosticContainer, "failures": diagnosticContainer, "items": diagnosticContainer,
	"summary": diagnosticContainer, "relay": diagnosticContainer, "details": diagnosticContainer,
	"checks": diagnosticContainer, "orphans": diagnosticContainer,
	"bySource": diagnosticDynamicNumberMap,
}

var knownDiagnosticSources = map[string]struct{}{
	"cnb": {}, "github": {}, "agentsmirror": {}, "gh-proxy": {}, "cdn-gh-proxy": {},
	"edgeone-gh-proxy": {}, "astral": {}, "aliyun": {}, "tsinghua": {}, "ustc": {}, "pypi": {},
}

// SanitizeDiagnostics 把结构化 details 转成有界、可上传的诊断副本。
func SanitizeDiagnostics(details map[string]any) map[string]any {
	fields := 0
	value := sanitizeDiagnosticValue(details, "", 0, &fields)
	clean, _ := value.(map[string]any)
	if clean == nil {
		clean = map[string]any{}
	}
	if encoded, err := json.Marshal(map[string]any{"details": clean}); err != nil || len(encoded) > maxDiagnosticJSONBytes {
		clean = boundedDiagnosticMap(clean)
	}
	return clean
}

func sanitizeDiagnosticValue(value any, key string, depth int, fields *int) any {
	if *fields >= maxDiagnosticFields {
		return nil
	}
	if sensitiveDiagnosticKey(key) {
		return nil
	}
	if key == "" {
		typed, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		return sanitizeDiagnosticMap(typed, depth, fields)
	}
	policy, allowed := diagnosticFieldPolicies[key]
	if !allowed {
		return nil
	}
	switch policy {
	case diagnosticStableToken:
		typed, ok := diagnosticString(value)
		if !ok || !validStableToken(typed, maxDiagnosticStringBytes, true) {
			return nil
		}
		return typed
	case diagnosticIdentifier:
		typed, ok := diagnosticString(value)
		if !ok || !validDiagnosticIdentifier(typed) {
			return nil
		}
		return typed
	case diagnosticSource:
		typed, ok := diagnosticString(value)
		if !ok || !knownDiagnosticSource(typed) {
			return nil
		}
		return typed
	case diagnosticStableValue:
		if _, numeric := value.(json.Number); numeric {
			return sanitizeDiagnosticNumber(value)
		}
		if typed, ok := diagnosticString(value); ok {
			if !validStableToken(typed, maxDiagnosticStringBytes, true) {
				return nil
			}
			return typed
		}
		return sanitizeDiagnosticNumber(value)
	case diagnosticPath:
		typed, ok := diagnosticString(value)
		if !ok {
			return nil
		}
		return sanitizeDiagnosticPath(typed)
	case diagnosticNumber:
		return sanitizeDiagnosticNumber(value)
	case diagnosticBoolean:
		typed, ok := diagnosticBool(value)
		if !ok {
			return nil
		}
		return typed
	case diagnosticContainer:
		if depth >= maxDiagnosticDepth {
			return nil
		}
		return sanitizeDiagnosticContainer(value, depth+1, fields)
	case diagnosticDynamicNumberMap:
		if depth >= maxDiagnosticDepth {
			return nil
		}
		return sanitizeDynamicNumberMap(value, depth+1, fields)
	case diagnosticProtocolCode:
		typed, ok := diagnosticString(value)
		if !ok || !protocol.IsKnownCode(protocol.Code(typed)) {
			return nil
		}
		return typed
	case diagnosticSHA256:
		typed, ok := diagnosticString(value)
		if !ok || !validDiagnosticSHA256(typed) {
			return nil
		}
		return strings.ToLower(typed)
	default:
		return nil
	}
}

func sanitizeDiagnosticMap(typed map[string]any, depth int, fields *int) map[string]any {
	keys := make([]string, 0, len(typed))
	for childKey := range typed {
		if validDiagnosticKey(childKey) {
			keys = append(keys, childKey)
		}
	}
	sort.Strings(keys)
	result := make(map[string]any, len(keys))
	for _, childKey := range keys {
		if *fields >= maxDiagnosticFields {
			break
		}
		if sensitiveDiagnosticKey(childKey) {
			continue
		}
		if _, allowed := diagnosticFieldPolicies[childKey]; !allowed {
			continue
		}
		(*fields)++
		if child := sanitizeDiagnosticValue(typed[childKey], childKey, depth, fields); child != nil {
			result[childKey] = child
		}
	}
	return result
}

func sanitizeDiagnosticContainer(value any, depth int, fields *int) any {
	if depth > maxDiagnosticDepth {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeDiagnosticMap(typed, depth, fields)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			converted[childKey] = childValue
		}
		return sanitizeDiagnosticMap(converted, depth, fields)
	case []any:
		return sanitizeDiagnosticSlice(typed, depth, fields)
	case []map[string]any:
		converted := make([]any, len(typed))
		for i, item := range typed {
			converted[i] = item
		}
		return sanitizeDiagnosticSlice(converted, depth, fields)
	case []string:
		converted := make([]any, len(typed))
		for i, item := range typed {
			converted[i] = item
		}
		return sanitizeDiagnosticSlice(converted, depth, fields)
	default:
		return nil
	}
}

func sanitizeDiagnosticSlice(values []any, depth int, fields *int) []any {
	limit := len(values)
	if limit > maxDiagnosticSliceItems {
		limit = maxDiagnosticSliceItems
	}
	result := make([]any, 0, limit)
	for _, item := range values[:limit] {
		if *fields >= maxDiagnosticFields {
			break
		}
		var clean any
		switch typed := item.(type) {
		case nil, bool:
			clean = typed
		case string:
			if validStableToken(typed, maxDiagnosticStringBytes, true) {
				clean = typed
			}
		case map[string]any:
			clean = sanitizeDiagnosticMap(typed, depth, fields)
		case map[string]string:
			converted := make(map[string]any, len(typed))
			for key, value := range typed {
				converted[key] = value
			}
			clean = sanitizeDiagnosticMap(converted, depth, fields)
		default:
			clean = sanitizeDiagnosticNumber(typed)
		}
		if clean != nil {
			result = append(result, clean)
		}
	}
	return result
}

func sanitizeDynamicNumberMap(value any, depth int, fields *int) map[string]any {
	if depth > maxDiagnosticDepth {
		return nil
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(typed))
	for key := range typed {
		if validDiagnosticKey(key) && !sensitiveDiagnosticKey(key) && knownDiagnosticSource(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		if *fields >= maxDiagnosticFields {
			break
		}
		clean := sanitizeDiagnosticNumber(typed[key])
		if clean == nil {
			continue
		}
		(*fields)++
		result[key] = clean
	}
	return result
}

func sanitizeDiagnosticNumber(value any) any {
	if hasCustomDiagnosticMarshaler(value) {
		return nil
	}
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return typed
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return nil
		}
		return typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil
		}
		return typed
	case json.Number:
		if _, err := typed.Float64(); err != nil {
			return nil
		}
		return typed
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return nil
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflected.Uint()
	case reflect.Float32, reflect.Float64:
		number := reflected.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil
		}
		return number
	default:
		return nil
	}
}

func diagnosticString(value any) (string, bool) {
	if hasCustomDiagnosticMarshaler(value) {
		return "", false
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.String {
		return "", false
	}
	return reflected.String(), true
}

func diagnosticBool(value any) (bool, bool) {
	if hasCustomDiagnosticMarshaler(value) {
		return false, false
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Bool {
		return false, false
	}
	return reflected.Bool(), true
}

func hasCustomDiagnosticMarshaler(value any) bool {
	typeOf := reflect.TypeOf(value)
	if typeOf == nil {
		return false
	}
	jsonMarshaler := reflect.TypeFor[json.Marshaler]()
	textMarshaler := reflect.TypeFor[encoding.TextMarshaler]()
	if typeOf.Implements(jsonMarshaler) || typeOf.Implements(textMarshaler) {
		return true
	}
	return typeOf.Kind() != reflect.Pointer &&
		(reflect.PointerTo(typeOf).Implements(jsonMarshaler) || reflect.PointerTo(typeOf).Implements(textMarshaler))
}

func validDiagnosticSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func sanitizeDiagnosticPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return ""
	}
	base := filepath.Base(value)
	if base == "." || base == "/" {
		return "[path]"
	}
	if !validDiagnosticFilename(base) {
		return "[path]"
	}
	return base
}

func knownDiagnosticSource(value string) bool {
	_, known := knownDiagnosticSources[value]
	return known
}

func validDiagnosticIdentifier(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value ||
		strings.Contains(value, "..") || strings.Contains(value, "//") {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' ||
			character == '-' || character == '+' || character == '/') {
			return false
		}
	}
	return true
}

func validDiagnosticFilename(value string) bool {
	if value == "" || len(value) > maxDiagnosticStringBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' ||
			character == '-' || character == '+') {
			return false
		}
	}
	return true
}

func sensitiveDiagnosticKey(key string) bool {
	lower := strings.ToLower(key)
	for _, marker := range []string{"token", "secret", "password", "authorization", "cookie", "credential", "dsn"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validDiagnosticKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, character := range key {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '_' && character != '-' && character != '.' {
			return false
		}
	}
	return true
}

func boundedDiagnosticMap(values map[string]any) map[string]any {
	result := map[string]any{"truncated": true}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = values[key]
		encoded, err := json.Marshal(map[string]any{"details": result})
		if err != nil || len(encoded) > maxDiagnosticJSONBytes {
			delete(result, key)
		}
	}
	return result
}
