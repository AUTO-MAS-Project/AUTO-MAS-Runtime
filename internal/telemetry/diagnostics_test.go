package telemetry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type unsupportedDiagnostic struct {
	Secret string
}

type diagnosticMode string

type marshaledDiagnostic string

func (m marshaledDiagnostic) MarshalJSON() ([]byte, error) {
	return json.Marshal("secret:" + string(m))
}

func TestSanitizeDiagnostics_RedactsSecretsAndBoundsPayload(t *testing.T) {
	long := strings.Repeat("x", maxDiagnosticStringBytes+100)
	details := SanitizeDiagnostics(map[string]any{
		"path":     `C:\Users\alice\runtime\update.json`,
		"password": "super-secret",
		"url":      "https://user:pass@example.test/repo?q=secret#fragment",
		"message":  long,
	})
	encoded, err := json.Marshal(map[string]any{"details": details})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"alice", "super-secret", "user:pass", "q=secret", `C:\\Users`} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("sanitized diagnostics contain %q: %s", forbidden, serialized)
		}
	}
	if details["path"] != "update.json" {
		t.Fatalf("details = %#v, want basename", details)
	}
	if _, ok := details["password"]; ok {
		t.Fatalf("details[password] = %#v, want credential field dropped", details["password"])
	}
	if len(encoded) > maxDiagnosticJSONBytes {
		t.Fatalf("diagnostic payload bytes = %d, want bounded", len(encoded))
	}
}

func TestSanitizeDiagnostics_RedactsCredentialEncodingsAndUnknownTypes(t *testing.T) {
	details := SanitizeDiagnostics(map[string]any{
		"path":     `D:\Github\secret`,
		"message":  `Cookie: session=secret; refresh=secret`,
		"password": "plain-secret",
		"unknown":  "must-drop",
		"details":  unsupportedDiagnostic{Secret: "must-drop"},
	})
	encoded, err := json.Marshal(map[string]any{"details": details})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{
		`D:\\Github\\secret`, "session=secret", "plain-secret", "must-drop",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("sanitized diagnostics contain %q: %s", forbidden, serialized)
		}
	}
	if _, ok := details["password"]; ok {
		t.Fatalf("details[password] = %#v, want credential field dropped", details["password"])
	}
	if _, ok := details["details"]; ok {
		t.Fatalf("details[details] = %#v, want unsupported struct dropped", details["details"])
	}
}

func TestSanitizeDiagnostics_PreservesRegisteredOperationalFacts(t *testing.T) {
	details := SanitizeDiagnostics(map[string]any{
		"code":              protocol.CodeGitRepoCleanupFailed,
		"mode":              diagnosticMode("managed"),
		"state":             "environment_broken",
		"sink":              "runtime_log",
		"version":           "v1.2.3",
		"expectedSHA256":    strings.Repeat("a", 64),
		"pid":               321,
		"previousPid":       123,
		"port":              36163,
		"consecutiveErrors": 3,
		"got":               json.Number("2"),
	})
	for key, want := range map[string]any{
		"code":              string(protocol.CodeGitRepoCleanupFailed),
		"mode":              "managed",
		"state":             "environment_broken",
		"sink":              "runtime_log",
		"version":           "v1.2.3",
		"expectedSHA256":    strings.Repeat("a", 64),
		"pid":               321,
		"previousPid":       123,
		"port":              36163,
		"consecutiveErrors": 3,
		"got":               json.Number("2"),
	} {
		if details[key] != want {
			t.Errorf("details[%q] = %#v, want %#v", key, details[key], want)
		}
	}
}

func TestSanitizeDiagnostics_DropsCustomScalarMarshalers(t *testing.T) {
	details := SanitizeDiagnostics(map[string]any{
		"mode": marshaledDiagnostic("managed"),
	})
	if _, ok := details["mode"]; ok {
		t.Fatalf("details[mode] = %#v, want custom marshaler dropped", details["mode"])
	}
}

func TestSanitizeDiagnostics_RejectsEncodedSecretsInSemanticFields(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "percent encoded JSON", value: `%22token%22%3A%22super-secret%22`},
		{name: "escaped JSON", value: `\{\"token\":\"super-secret\"\}`},
		{name: "assignment", value: `api_key=super-secret`},
		{name: "email", value: `alice@example.test`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			details := SanitizeDiagnostics(map[string]any{
				"source":      test.value,
				"failureKind": test.value,
				"branch":      test.value,
			})
			encoded, err := json.Marshal(details)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if strings.Contains(string(encoded), "super-secret") || len(details) != 0 {
				t.Fatalf("semantic diagnostics = %s, want encoded secret fields dropped", encoded)
			}
		})
	}
}

func TestSanitizeDiagnostics_BySourceOnlyKeepsKnownSources(t *testing.T) {
	details := SanitizeDiagnostics(map[string]any{
		"bySource": map[string]any{
			"aliyun":                    int64(1024),
			"access_token-super-secret": int64(2048),
			"unknown-source":            int64(4096),
		},
	})
	bySource, ok := details["bySource"].(map[string]any)
	if !ok {
		t.Fatalf("details[bySource] = %#v, want object", details["bySource"])
	}
	if len(bySource) != 1 || bySource["aliyun"] != int64(1024) {
		t.Fatalf("bySource = %#v, want only known aliyun source", bySource)
	}
}

func TestKnownDiagnosticSources_MatchesDefaultCatalog(t *testing.T) {
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	want := make(map[string]struct{})
	for _, kind := range mirror.AllKinds() {
		for _, source := range catalog.Sources(kind) {
			want[source.Key()] = struct{}{}
		}
	}
	if len(knownDiagnosticSources) != len(want) {
		t.Fatalf("knownDiagnosticSources = %#v, want catalog union %#v", knownDiagnosticSources, want)
	}
	for key := range want {
		if !knownDiagnosticSource(key) {
			t.Fatalf("knownDiagnosticSource(%q) = false, want true", key)
		}
	}
}

func TestSanitizeDiagnostics_PreservesProductionTypedContainers(t *testing.T) {
	details := SanitizeDiagnostics(map[string]any{
		"attempts": []map[string]any{{
			"source": "github", "sourceTry": 1, "globalTry": 2,
			"outcome": "failed", "failureKind": "network",
		}},
		"failures": []map[string]any{{"item": "package.whl", "attempts": []map[string]any{{"source": "aliyun", "outcome": "failed"}}}},
		"checks":   []string{"lockfile", "environment"},
		"exitCode": json.Number("7"),
		"relay": map[string]any{
			"files":    2,
			"bytes":    4096,
			"bySource": map[string]any{"aliyun": int64(4096)},
			"failures": []map[string]any{{
				"item":     "package.whl",
				"attempts": []map[string]any{{"source": "aliyun", "outcome": "failed"}},
			}},
		},
		"items": []map[string]any{{
			"id": "runtime-update", "status": "failed",
			"details": map[string]any{"operation": "remove", "path": `C:\runtime\repo.update`},
		}},
	})
	attempts, ok := details["attempts"].([]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("details[attempts] = %#v, want one canonical JSON slice", details["attempts"])
	}
	first, ok := attempts[0].(map[string]any)
	if !ok || first["source"] != "github" || first["failureKind"] != "network" {
		t.Fatalf("first attempt = %#v, want useful source and failure kind", attempts[0])
	}
	if checks, ok := details["checks"].([]any); !ok || len(checks) != 2 {
		t.Fatalf("details[checks] = %#v, want canonical string slice", details["checks"])
	}
	if details["exitCode"] != json.Number("7") {
		t.Fatalf("details[exitCode] = %#v, want json.Number(7)", details["exitCode"])
	}
	relayDetails, ok := details["relay"].(map[string]any)
	if !ok {
		t.Fatalf("details[relay] = %#v, want object", details["relay"])
	}
	relayFailures, ok := relayDetails["failures"].([]any)
	if !ok || len(relayFailures) != 1 {
		t.Fatalf("relay failures = %#v, want one item", relayDetails["failures"])
	}
	relayFailure := relayFailures[0].(map[string]any)
	relayAttempts, ok := relayFailure["attempts"].([]any)
	if !ok || len(relayAttempts) != 1 || relayAttempts[0].(map[string]any)["source"] != "aliyun" {
		t.Fatalf("relay attempts = %#v, want preserved source", relayFailure["attempts"])
	}
	items, ok := details["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["status"] != "failed" {
		t.Fatalf("cleanup items = %#v, want preserved status", details["items"])
	}
}

func TestSanitizeDiagnostics_BoundsCompletePayload(t *testing.T) {
	values := make([]string, maxDiagnosticSliceItems)
	for i := range values {
		values[i] = strings.Repeat("x", maxDiagnosticStringBytes)
	}
	details := SanitizeDiagnostics(map[string]any{
		"checks": values,
	})
	encoded, err := json.Marshal(map[string]any{"details": details})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if len(encoded) > maxDiagnosticJSONBytes {
		t.Fatalf("diagnostic payload bytes = %d, want <= %d", len(encoded), maxDiagnosticJSONBytes)
	}
}
