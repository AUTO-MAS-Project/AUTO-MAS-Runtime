package gitrepo

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestAttemptDetails_TelemetryPreservesProductionShape(t *testing.T) {
	details := attemptDetails([]mirror.AttemptReport{{
		SourceKey:   "github",
		SourceTry:   1,
		GlobalTry:   2,
		Outcome:     mirror.OutcomeSwitchSource,
		FailureKind: mirror.FailureKind("network"),
	}})
	clean := telemetry.SanitizeDiagnostics(details)
	attempts, ok := clean["attempts"].([]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("telemetry attempts = %#v, want one item", clean["attempts"])
	}
	attempt, ok := attempts[0].(map[string]any)
	if !ok || attempt["source"] != "github" || attempt["outcome"] != "switch_source" ||
		attempt["failureKind"] != "network" || attempt["sourceTry"] != 1 || attempt["globalTry"] != 2 {
		t.Fatalf("telemetry attempt = %#v, want production fields preserved", attempts[0])
	}
}
