package doctor

import (
	"context"
	"testing"
)

func TestProbeDiskFree_Production(t *testing.T) {
	t.Parallel()
	probes := ProductionProbes()
	if probes.UVVersion == nil || probes.DiskFree == nil {
		t.Fatal("ProductionProbes() returned nil probe functions")
	}
	free, err := probes.DiskFree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("DiskFree() error = %v", err)
	}
	if free == 0 {
		t.Error("DiskFree() = 0, want positive free bytes")
	}
}

func TestProbeDiskFree_RejectsEmptyPath(t *testing.T) {
	t.Parallel()
	probes := ProductionProbes()
	if _, err := probes.DiskFree(context.Background(), ""); err == nil {
		t.Fatal("DiskFree(\"\") error = nil, want error")
	}
}
