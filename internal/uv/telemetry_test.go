package uv

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestRelaySummaryDetails_TelemetryPreservesProductionShape(t *testing.T) {
	details := map[string]any{"relay": RelaySummaryDetails(relay.Summary{
		Files:    2,
		Bytes:    4096,
		BySource: map[string]int64{"aliyun": 4096},
		Failures: []relay.FileFailure{{
			Item: `C:\cache\package.whl`,
			Attempts: []relay.AttemptOutcome{{
				Source:  "aliyun",
				Outcome: relay.OutcomeHTTPStatus,
			}},
		}},
	})}
	clean := telemetry.SanitizeDiagnostics(details)
	relayDetails, ok := clean["relay"].(map[string]any)
	if !ok || relayDetails["files"] != 2 || relayDetails["bytes"] != int64(4096) {
		t.Fatalf("telemetry relay = %#v, want stable summary", clean["relay"])
	}
	bySource, ok := relayDetails["bySource"].(map[string]any)
	if !ok || bySource["aliyun"] != int64(4096) {
		t.Fatalf("telemetry bySource = %#v, want known source bytes", relayDetails["bySource"])
	}
	failures := relayDetails["failures"].([]any)
	failure := failures[0].(map[string]any)
	attempts := failure["attempts"].([]any)
	attempt := attempts[0].(map[string]any)
	if failure["item"] != "package.whl" || attempt["source"] != "aliyun" || attempt["outcome"] != "http_status" {
		t.Fatalf("telemetry relay failure = %#v/%#v, want item and attempt", failure, attempt)
	}
}
