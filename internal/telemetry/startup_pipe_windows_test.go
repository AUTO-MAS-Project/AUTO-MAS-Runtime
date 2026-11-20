package telemetry

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"github.com/Microsoft/go-winio"
	"testing"
)

// 桌面端测试替身只通过一次性本机管道提供摘要，不触碰生产遥测。
func TestStartupResources_DesktopPipe(t *testing.T) {
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
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		request, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && request == token[32:]+"\n" {
			_, err = conn.Write([]byte(`{"cpu_peak":"90-100%","path":"private"}`))
		}
		done <- err
	}()
	fields := readStartupResources()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["cpu_peak"] != "90-100%" {
		t.Fatalf("fields = %v, want allowed CPU band", fields)
	}
	t.Setenv("AUTO_MAS_STARTUP_SAMPLE", "not-an-invitation")
	if got := readStartupResources(); len(got) != 0 {
		t.Fatalf("without invitation = %v, want empty", got)
	}
}
