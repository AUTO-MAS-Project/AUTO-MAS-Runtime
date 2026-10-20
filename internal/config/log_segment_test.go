package config_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestLayout_RuntimeLogSegmentFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	for _, test := range []struct {
		name     string
		sequence uint64
	}{
		{name: "backend supervise-20261020.log"},
		{name: "backend supervise-20261020.001.log", sequence: 1},
		{name: "backend supervise-20261020.999.log", sequence: 999},
		{name: "backend supervise-20261020.1000.log", sequence: 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := layout.RuntimeLogSegmentFile("backend supervise", now, test.sequence)
			if err != nil || path != filepath.Join(layout.RuntimeLogDir(), test.name) {
				t.Fatalf("RuntimeLogSegmentFile() = %q, %v, want %q", path, err, test.name)
			}
		})
	}
	if _, err := layout.RuntimeLogSegmentFile("../outside", now, 1); err == nil {
		t.Fatal("RuntimeLogSegmentFile() accepted path escape")
	}
	if _, err := layout.RuntimeLogSegmentFile("doctor", time.Time{}, 1); err == nil {
		t.Fatal("RuntimeLogSegmentFile() accepted zero date")
	}
}
