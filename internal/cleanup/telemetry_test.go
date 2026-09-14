package cleanup

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry"
)

func TestReportDetails_TelemetryPreservesProductionShape(t *testing.T) {
	details := ReportDetails(Report{
		Items: []Item{
			{
				ID:     "runtime-update",
				Status: ItemFailed,
				Details: map[string]any{
					"operation": "remove",
					"path":      `C:\runtime\repo.update-1`,
				},
			},
			{
				ID:      "repo",
				Status:  ItemFailed,
				Details: map[string]any{"code": "GIT_REPO_CLEANUP_FAILED"},
			},
		},
		Summary: Summary{Total: 2, Failed: 2},
	})
	clean := telemetry.SanitizeDiagnostics(details)
	items, ok := clean["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("telemetry items = %#v, want two items", clean["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["id"] != "runtime-update" || item["status"] != "failed" {
		t.Fatalf("telemetry item = %#v, want stable id and status", items[0])
	}
	itemDetails, ok := item["details"].(map[string]any)
	if !ok || itemDetails["operation"] != "remove" || itemDetails["path"] != "repo.update-1" {
		t.Fatalf("telemetry item details = %#v, want operation and basename", item["details"])
	}
	codeDetails := items[1].(map[string]any)["details"].(map[string]any)
	if codeDetails["code"] != "GIT_REPO_CLEANUP_FAILED" {
		t.Fatalf("telemetry cleanup code = %#v, want stable code", codeDetails["code"])
	}
	summary, ok := clean["summary"].(map[string]any)
	if !ok || summary["total"] != 2 || summary["failed"] != 2 {
		t.Fatalf("telemetry summary = %#v, want stable counts", clean["summary"])
	}
}
