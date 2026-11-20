package telemetry

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func serveStartupSample(t *testing.T, json string) {
	t.Helper()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret[:])
	t.Setenv("AUTO_MAS_STARTUP_SAMPLE", token)
	listener, err := winio.ListenPipe(`\\.\pipe\auto-mas-startup-`+token[:32], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		request, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && request == token[32:]+"\n" {
			_, _ = conn.Write([]byte(json))
		}
	}()
}

func TestStartupResources_NoActiveSample(t *testing.T) {
	for _, body := range []string{`{}`, `{"sample_age":"60s+","coverage":"sampled","cpu_peak":"90-100%"}`, `{"sample_age":"<1s","coverage":"insufficient"}`} {
		serveStartupSample(t, body)
		if got := readStartupResources(); len(got) != 0 {
			t.Fatalf("resources = %v, want none for %s", got, body)
		}
	}
	t.Setenv("AUTO_MAS_STARTUP_SAMPLE", "not-an-invitation")
	if got := readStartupResources(); len(got) != 0 {
		t.Fatalf("without invitation = %v, want empty", got)
	}
}
