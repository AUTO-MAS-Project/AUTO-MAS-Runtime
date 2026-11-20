package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStartupResources_LocalEnvelope 通过 SDK 的 HTTPS 传输读取最终 envelope。
func TestStartupResources_LocalEnvelope(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var source io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			defer gz.Close()
			source = gz
		}
		payload, err := io.ReadAll(io.LimitReader(source, 128*1024))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		bodies <- payload
		w.WriteHeader(200)
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = old }()
	candidate, err := newSentryProvider(Config{Enabled: true, SentryDSN: strings.Replace(server.URL, "https://", "https://public@", 1) + "/1", SentryRelease: "v1.0.0", SentryEnvironment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	serveStartupSample(t, `{"coverage":"sampled","sample_age":"<1s","install_free":"<1GiB","path":"do-not-send-private"}`)
	candidate.captureInternal(validSentryObservation(true))
	candidate.close(context.Background())
	select {
	case data := <-bodies:
		if bytes.Contains(data, []byte("do-not-send-private")) {
			t.Fatal("unexpected private value in envelope")
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var event struct {
				Contexts map[string]map[string]any `json:"contexts"`
			}
			if json.Unmarshal(line, &event) == nil && event.Contexts["startup_resources"]["install_free"] == "<1GiB" {
				return
			}
		}
		t.Fatal("startup_resources missing from envelope")
	case <-time.After(3 * time.Second):
		t.Fatal("local Sentry receiver timed out")
	}
}
