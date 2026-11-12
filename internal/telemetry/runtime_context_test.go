package telemetry

import (
	"reflect"
	"testing"
)

func TestSanitizeDiagnostics_RuntimeContext(t *testing.T) {
	got := SanitizeDiagnostics(map[string]any{"fileSystem": "exFAT", "unlinkMode": "classic", "logPath": `C:\private\backend supervise-20261112.log`})
	want := map[string]any{"fileSystem": "exFAT", "unlinkMode": "classic", "logPath": "backend supervise-20261112.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, value := range []string{"private words.log", "backend supervise-private.log"} {
		if got := SanitizeDiagnostics(map[string]any{"logPath": value}); got["logPath"] != "[path]" {
			t.Fatalf("got %#v, want redacted", got)
		}
	}
	if got := SanitizeDiagnostics(map[string]any{"fileSystem": "private", "unlinkMode": "private"}); len(got) != 0 {
		t.Fatalf("got %#v, want empty", got)
	}
}

func TestSentry_CorrelationReceipt(t *testing.T) {
	p, transport := newSentryTestProvider(t)
	observation := validSentryObservation(false)
	id := "01J00000000000000000000000"
	observation.OperationID = id
	receipt := p.captureInternal(observation)
	_, events, _, _ := transport.snapshot()
	if len(events) != 1 || receipt == "" || receipt != string(events[0].EventID) || events[0].Tags["operation_id"] != id {
		t.Fatalf("got receipt=%q events=%#v, want correlated event", receipt, events)
	}
	observation.OperationID = "private-token"
	if receipt := p.captureInternal(observation); receipt != "" {
		t.Fatal("accepted invalid operation ID")
	}
}
