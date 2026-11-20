package telemetry

import (
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"testing"
)

func TestStartupResources_FinalEvent(t *testing.T) {
	for _, kind := range []string{"failure", "internal", "panic"} {
		t.Run(kind, func(t *testing.T) {
			provider, transport := newSentryTestProvider(t)
			obs := validSentryObservation(kind == "panic")
			if kind == "failure" {
				obs.Code = string(protocol.CodeDependencySyncFailed)
			}
			obs.StartupResources = map[string]any{"cpu_peak": "90-100%", "install_free": "<1GiB", "install_fs": "private", "path": "private", "gpu0_series": map[string]any{"secret": "private"}}
			provider.captureInternal(obs)
			_, events, _, _ := transport.snapshot()
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			got := events[0].Contexts["startup_resources"]
			if len(got) != 2 || got["cpu_peak"] != "90-100%" || got["install_free"] != "<1GiB" {
				t.Fatalf("startup resources = %v, want two allowed fields", got)
			}
		})
	}
}
