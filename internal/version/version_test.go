package version

import (
	"context"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestVersion_DefaultPlaceholders(t *testing.T) {
	t.Parallel()
	info, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if info.Version != "dev" {
		t.Errorf("Version = %q, want dev placeholder", info.Version)
	}
	if info.Commit != "" {
		t.Errorf("Commit = %q, want empty placeholder", info.Commit)
	}
	if info.BuildDate != "" {
		t.Errorf("BuildDate = %q, want empty placeholder", info.BuildDate)
	}
}

func TestVersion_InfoIncludesProtocolAndGoVersion(t *testing.T) {
	t.Parallel()
	info, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if info.Protocol != protocol.Version {
		t.Errorf("Protocol = %d, want %d", info.Protocol, protocol.Version)
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Errorf("GoVersion = %q, want go prefix", info.GoVersion)
	}
}

func TestVersion_LoadRespectsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx); err == nil {
		t.Fatal("Load() error = nil, want cancelled context error")
	}
}
