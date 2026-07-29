package config_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestLayout_DynamicPathsPreserveSegments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}

	update, err := layout.RepoUpdateDir("Op-ID_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "repo.update-Op-ID_1"); update != want {
		t.Fatalf("RepoUpdateDir() = %q, want %q", update, want)
	}

	previous, err := layout.RepoPreviousDir("Op-ID_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "repo.previous-Op-ID_1"); previous != want {
		t.Fatalf("RepoPreviousDir() = %q, want %q", previous, want)
	}

	versionDir, err := layout.UVVersionDir("0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(layout.UVToolsDir(), "0.8.0"); versionDir != want {
		t.Fatalf("UVVersionDir() = %q, want %q", versionDir, want)
	}

	executable, err := layout.UVExecutable("0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(versionDir, "uv.exe"); executable != want {
		t.Fatalf("UVExecutable() = %q, want %q", executable, want)
	}

	download, err := layout.DownloadFile("uv-x86_64.zip")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(layout.DownloadCacheDir(), "uv-x86_64.zip"); download != want {
		t.Fatalf("DownloadFile() = %q, want %q", download, want)
	}

	part, err := layout.DownloadPartFile("uv-x86_64.zip")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(layout.DownloadCacheDir(), "uv-x86_64.zip.part"); part != want {
		t.Fatalf("DownloadPartFile() = %q, want %q", part, want)
	}

	staging, err := layout.UVStagingDir("0.8.0", "Op-ID_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(layout.BuildCacheDir(), "uv", "0.8.0", "Op-ID_1"); staging != want {
		t.Fatalf("UVStagingDir() = %q, want %q", staging, want)
	}
}

func TestLayout_DynamicPathsRejectUnsafeSegments(t *testing.T) {
	layout := newDynamicPathsLayout(t)
	unsafe := []string{
		"", ".", "..", "a/b", `a\b`, "C:relative",
		"bad<name", "bad>name", `bad"name`, "bad|name", "bad?name",
		"bad*name", "bad:name", "trailing.", "trailing ",
		"CON", "con.txt", "PRN", "AUX.log", "NUL",
		"CONIN$", "conin$.txt", "CONOUT$", "conout$.log",
		"COM1", "COM9.bin", "LPT1", "lpt9.txt",
		strings.Repeat("a", 129),
		strings.Repeat("\U0001F600", 65),
	}
	for control := rune(0); control <= '\x1f'; control++ {
		unsafe = append(unsafe, "safe"+string(control)+"name")
	}

	for _, value := range unsafe {
		t.Run(segmentTestName(value), func(t *testing.T) {
			assertInvalidSegment(t, "RepoUpdateDir", func() (string, error) {
				return layout.RepoUpdateDir(value)
			})
			assertInvalidSegment(t, "RepoPreviousDir", func() (string, error) {
				return layout.RepoPreviousDir(value)
			})
			assertInvalidSegment(t, "UVVersionDir", func() (string, error) {
				return layout.UVVersionDir(value)
			})
			assertInvalidSegment(t, "UVExecutable", func() (string, error) {
				return layout.UVExecutable(value)
			})
			assertInvalidSegment(t, "DownloadFile", func() (string, error) {
				return layout.DownloadFile(value)
			})
			assertInvalidSegment(t, "DownloadPartFile", func() (string, error) {
				return layout.DownloadPartFile(value)
			})
			assertInvalidSegment(t, "RuntimeLogFile", func() (string, error) {
				return layout.RuntimeLogFile(value, time.Date(2026, 7, 30, 0, 30, 0, 0, time.UTC))
			})
		})

		t.Run("staging version "+segmentTestName(value), func(t *testing.T) {
			assertInvalidSegment(t, "UVStagingDir", func() (string, error) {
				return layout.UVStagingDir(value, "Op-ID_1")
			})
		})
		t.Run("staging operation id "+segmentTestName(value), func(t *testing.T) {
			assertInvalidSegment(t, "UVStagingDir", func() (string, error) {
				return layout.UVStagingDir("0.8.0", value)
			})
		})
	}
}

func TestLayout_DynamicSegmentUTF16LengthBoundary(t *testing.T) {
	layout := newDynamicPathsLayout(t)
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "128 ASCII code units", value: strings.Repeat("a", 128)},
		{name: "129 ASCII code units", value: strings.Repeat("a", 129), wantErr: true},
		{name: "64 supplementary runes are 128 units", value: strings.Repeat("\U0001F600", 64)},
		{name: "65 supplementary runes are 130 units", value: strings.Repeat("\U0001F600", 65), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := layout.DownloadFile(test.value)
			if test.wantErr {
				if got != "" {
					t.Fatalf("DownloadFile() path = %q, want empty path", got)
				}
				if !errors.Is(err, config.ErrInvalidSegment) {
					t.Fatalf("DownloadFile() error = %v, want errors.Is(_, %v)", err, config.ErrInvalidSegment)
				}
				return
			}
			if err != nil {
				t.Fatalf("DownloadFile() error = %v", err)
			}
			if want := filepath.Join(layout.DownloadCacheDir(), test.value); got != want {
				t.Fatalf("DownloadFile() = %q, want %q", got, want)
			}
		})
	}
}

func TestLayout_RuntimeLogFileUsesLocalDate(t *testing.T) {
	layout := newDynamicPathsLayout(t)
	local := time.Date(2026, 7, 30, 0, 30, 0, 0, time.FixedZone("CST", 8*60*60))

	got, err := layout.RuntimeLogFile("workspace-sync", local)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(layout.RuntimeLogDir(), "workspace-sync-20260730.log"); got != want {
		t.Fatalf("RuntimeLogFile() = %q, want %q", got, want)
	}
}

func TestLayout_RuntimeLogFileRejectsZeroDate(t *testing.T) {
	layout := newDynamicPathsLayout(t)
	got, err := layout.RuntimeLogFile("workspace-sync", time.Time{})
	if got != "" {
		t.Fatalf("RuntimeLogFile() path = %q, want empty path", got)
	}
	if !errors.Is(err, config.ErrInvalidLogDate) {
		t.Fatalf("RuntimeLogFile() error = %v, want errors.Is(_, %v)", err, config.ErrInvalidLogDate)
	}
}

func assertInvalidSegment(t *testing.T, name string, call func() (string, error)) {
	t.Helper()
	got, err := call()
	if got != "" {
		t.Fatalf("%s() path = %q, want empty path", name, got)
	}
	if !errors.Is(err, config.ErrInvalidSegment) {
		t.Fatalf("%s() error = %v, want errors.Is(_, %v)", name, err, config.ErrInvalidSegment)
	}
}

func newDynamicPathsLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "AUTO-MAS")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

func segmentTestName(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\x00", "NUL"), "\n", "LF")
}
