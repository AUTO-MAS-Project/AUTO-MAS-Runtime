package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// TestProbeProgress_BytesPerSecondTriState 锁定增补 2 C18 第 2 条修订：吞吐三态——
// 正数为实测、0 为失败、只测首字节的成功探测不带该字段。
func TestProbeProgress_BytesPerSecondTriState(t *testing.T) {
	var stdout bytes.Buffer
	output, err := protocol.NewProcessOutput(nopFlusher{&stdout})
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "bootstrap", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	github, _ := catalog.Source(mirror.KindGit, "github")
	report := probeProgress(emitter)
	cases := []struct {
		name   string
		result mirror.ProbeResult
		want   *int64
	}{
		{name: "ttfb only success", result: mirror.ProbeResult{Source: github, OK: true, TTFB: 180 * time.Millisecond}, want: nil},
		{name: "failure", result: mirror.ProbeResult{Source: github, OK: false}, want: int64Ptr(0)},
		{name: "measured", result: mirror.ProbeResult{Source: github, OK: true, Bytes: 262144, BytesPerSecond: 5_000_000}, want: int64Ptr(5_000_000)},
	}
	for index, test := range cases {
		if err := report(mirror.KindGit, test.result, index+1, len(cases)); err != nil {
			t.Fatalf("%s: report error = %v", test.name, err)
		}
	}
	events := parseNDJSON(t, stdout.String())
	probes := make([]parsedEvent, 0, 3)
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) && eventString(event, "stage") == string(protocol.StageNetworkProbe) {
			probes = append(probes, event)
		}
	}
	if len(probes) != len(cases) {
		t.Fatalf("probe events = %d, want %d", len(probes), len(cases))
	}
	for index, test := range cases {
		value, present := probes[index].object["bytesPerSecond"]
		if test.want == nil {
			if present {
				t.Errorf("%s: bytesPerSecond = %v, want omitted", test.name, value)
			}
			continue
		}
		rate, _ := value.(float64)
		if !present || int64(rate) != *test.want {
			t.Errorf("%s: bytesPerSecond = %v, want %d", test.name, value, *test.want)
		}
	}
}

// TestUVDownloadProgress_CarriesItemSourceAndRate 锁定 uv.download 的 running 带制品名、来源与按时钟差算出的吞吐。
func TestUVDownloadProgress_CarriesItemSourceAndRate(t *testing.T) {
	var stdout bytes.Buffer
	output, err := protocol.NewProcessOutput(nopFlusher{&stdout})
	if err != nil {
		t.Fatalf("NewProcessOutput() error = %v", err)
	}
	emitter, err := output.NewEmitter("v1.0.0", "bootstrap", nil)
	if err != nil {
		t.Fatalf("NewEmitter() error = %v", err)
	}
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	progress := uvDownloadProgressWithClock(emitter, clock)
	steps := []mirror.DownloadProgress{
		{Received: 0, Total: 19013455, Percent: 0, Source: "edgeone-gh-proxy"},
		{Received: 4_000_000, Total: 19013455, Percent: 21.04, Source: "edgeone-gh-proxy"},
		{Received: 19013455, Total: 19013455, Percent: 100, Source: "edgeone-gh-proxy"},
	}
	for index, step := range steps {
		if index > 0 {
			now = now.Add(200 * time.Millisecond)
		}
		if err := progress(step); err != nil {
			t.Fatalf("progress(%d) error = %v", index, err)
		}
	}
	events := parseNDJSON(t, stdout.String())
	downloads := make([]parsedEvent, 0, 3)
	for _, event := range events {
		if eventType(event) == string(protocol.TypeProgress) && eventString(event, "stage") == string(protocol.StageUVDownload) {
			downloads = append(downloads, event)
		}
	}
	if len(downloads) != 3 {
		t.Fatalf("uv.download events = %d, want 3", len(downloads))
	}
	for _, event := range downloads {
		if eventString(event, "item") != uv.WindowsX64Artifact || eventString(event, "source") != "edgeone-gh-proxy" {
			t.Fatalf("event item/source = %q/%q", eventString(event, "item"), eventString(event, "source"))
		}
	}
	if _, present := downloads[0].object["bytesPerSecond"]; present {
		t.Fatalf("first event carries bytesPerSecond %v, want omitted before a delta exists", downloads[0].object["bytesPerSecond"])
	}
	rate, _ := downloads[1].object["bytesPerSecond"].(float64)
	if int64(rate) != 20_000_000 {
		t.Fatalf("second event bytesPerSecond = %v, want 20000000 (4 MB in 200 ms)", downloads[1].object["bytesPerSecond"])
	}
}

func int64Ptr(value int64) *int64 { return &value }
