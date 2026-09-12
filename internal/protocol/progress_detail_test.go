package protocol_test

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// TestProgressEvent_OptionalFieldsOmittedWhenEmpty 锁定增补 2 C18：三个细目字段缺失时
// 不出现在 NDJSON 里（不得输出 null），存在时按声明类型输出。
func TestProgressEvent_OptionalFieldsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	var output flushingBuffer
	emitter := newTestEmitter(t, &output)
	rate := int64(3_200_000)
	events := []protocol.ProgressEvent{
		{Stage: protocol.StageDependenciesSync, Status: protocol.ProgressRunning, Message: "plain"},
		{
			Stage:          protocol.StageDependenciesSync,
			Status:         protocol.ProgressRunning,
			Item:           "opencv_python-4.12.0.88-cp37-abi3-win_amd64.whl",
			Source:         "aliyun",
			BytesPerSecond: &rate,
			Message:        "detailed",
		},
	}
	for _, event := range events {
		if err := emitter.EmitProgress(event); err != nil {
			t.Fatalf("EmitProgress() error = %v", err)
		}
	}

	decoded := decodeEvents(t, output.String())
	if len(decoded) != 3 {
		t.Fatalf("decoded %d events, want hello + 2 progress", len(decoded))
	}
	plain := decoded[1]
	for _, field := range []string{"item", "source", "bytesPerSecond"} {
		if _, exists := plain[field]; exists {
			t.Errorf("plain progress has %q = %#v, want omitted", field, plain[field])
		}
	}
	detailed := decoded[2]
	if got := detailed["item"]; got != "opencv_python-4.12.0.88-cp37-abi3-win_amd64.whl" {
		t.Errorf("item = %#v", got)
	}
	if got := detailed["source"]; got != "aliyun" {
		t.Errorf("source = %#v", got)
	}
	if got := detailed["bytesPerSecond"]; got == nil || got.(interface{ String() string }).String() != "3200000" {
		t.Errorf("bytesPerSecond = %#v, want 3200000", got)
	}
}

// TestStage_NetworkProbeValid 锁定 network.probe 属于稳定 stage 集合。
func TestStage_NetworkProbeValid(t *testing.T) {
	t.Parallel()

	if protocol.StageNetworkProbe != protocol.Stage("network.probe") {
		t.Fatalf("StageNetworkProbe = %q", protocol.StageNetworkProbe)
	}
	if !protocol.IsKnownStage(protocol.StageNetworkProbe) {
		t.Error("IsKnownStage(network.probe) = false, want true")
	}
}
