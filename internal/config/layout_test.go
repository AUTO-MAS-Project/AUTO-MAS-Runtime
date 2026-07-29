package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestNewLayout_ResolvesExplicitBase(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name    string
		appRoot string
		want    string
	}{
		{name: "relative", appRoot: "AUTO-MAS", want: filepath.Join(base, "AUTO-MAS")},
		{name: "dot", appRoot: ".", want: filepath.Clean(base)},
		{
			name:    "parent and trailing separator",
			appRoot: filepath.Join("nested", "..", "AUTO-MAS") + string(filepath.Separator),
			want:    filepath.Join(base, "AUTO-MAS"),
		},
		{
			name:    "mixed separators with parent and trailing separator",
			appRoot: `nested/..\AUTO-MAS/`,
			want:    filepath.Join(base, "AUTO-MAS"),
		},
		{
			name:    "absolute",
			appRoot: filepath.Join(base, "AbsoluteRoot"),
			want:    filepath.Join(base, "AbsoluteRoot"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout, err := config.NewLayout(test.appRoot, base)
			if err != nil {
				t.Fatalf("NewLayout(%q, %q) error = %v", test.appRoot, base, err)
			}
			if got := layout.AppRoot(); got != filepath.Clean(test.want) {
				t.Fatalf("AppRoot() = %q, want %q", got, filepath.Clean(test.want))
			}
		})
	}
}

func TestNewLayout_RejectsInvalidInputs(t *testing.T) {
	base := t.TempDir()
	volumeRoot := filepath.VolumeName(base) + string(filepath.Separator)
	tests := []struct {
		name    string
		appRoot string
		base    string
		want    error
	}{
		{name: "empty app root", base: base, want: config.ErrEmptyPath},
		{name: "empty base", appRoot: "AUTO-MAS", want: config.ErrEmptyPath},
		{name: "nul app root", appRoot: "AUTO\x00-MAS", base: base, want: config.ErrPathContainsNUL},
		{name: "nul base", appRoot: "AUTO-MAS", base: base + "\x00", want: config.ErrPathContainsNUL},
		{name: "relative base", appRoot: "AUTO-MAS", base: "relative", want: config.ErrBaseNotAbsolute},
		{name: "volume relative app root", appRoot: "C:relative", base: base, want: config.ErrInvalidPath},
		{name: "volume root", appRoot: volumeRoot, base: base, want: config.ErrAppRootIsRoot},
		{name: "unc share root", appRoot: `\\server\share\`, base: base, want: config.ErrAppRootIsRoot},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.NewLayout(test.appRoot, test.base)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewLayout(%q, %q) error = %v, want errors.Is(_, %v)", test.appRoot, test.base, err, test.want)
			}
		})
	}
}

func TestNewLayout_DoesNotExpandShellSyntax(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name    string
		appRoot string
	}{
		{name: "tilde", appRoot: `~\AUTO-MAS`},
		{name: "environment variable", appRoot: `%LOCALAPPDATA%\AUTO-MAS`},
		{name: "shell variable", appRoot: `$HOME\AUTO-MAS`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout, err := config.NewLayout(test.appRoot, base)
			if err != nil {
				t.Fatalf("NewLayout(%q, %q) error = %v", test.appRoot, base, err)
			}
			if got, want := layout.AppRoot(), filepath.Join(base, test.appRoot); got != want {
				t.Fatalf("AppRoot() = %q, want %q", got, want)
			}
		})
	}
}

func TestNewLayout_DoesNotReadCurrentDirectory(t *testing.T) {
	base := t.TempDir()
	layout, err := config.NewLayout("AUTO-MAS", base)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	wantAppRoot := layout.AppRoot()
	wantRepoDir := layout.RepoDir()
	wantRepoUpdateDir, err := layout.RepoUpdateDir("Op-ID_1")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}

	originalWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWorkingDirectory); err != nil {
			t.Errorf("restore current directory: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	if got := layout.AppRoot(); got != wantAppRoot {
		t.Fatalf("AppRoot() = %q, want %q", got, wantAppRoot)
	}
	if got := layout.RepoDir(); got != wantRepoDir {
		t.Fatalf("RepoDir() = %q, want %q", got, wantRepoDir)
	}
	gotRepoUpdateDir, err := layout.RepoUpdateDir("Op-ID_1")
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	if gotRepoUpdateDir != wantRepoUpdateDir {
		t.Fatalf("RepoUpdateDir() = %q, want %q", gotRepoUpdateDir, wantRepoUpdateDir)
	}
}

func TestLayout_IdentityKeyIsCaseInsensitive(t *testing.T) {
	base := t.TempDir()
	upper, err := config.NewLayout("AUTO-MAS", base)
	if err != nil {
		t.Fatalf("NewLayout(upper) error = %v", err)
	}
	lower, err := config.NewLayout("auto-mas"+string(filepath.Separator), base)
	if err != nil {
		t.Fatalf("NewLayout(lower) error = %v", err)
	}
	if got, want := lower.IdentityKey(), upper.IdentityKey(); got != want {
		t.Fatalf("lower IdentityKey() = %q, want %q", got, want)
	}
	if got, want := upper.AppRoot(), filepath.Join(base, "AUTO-MAS"); got != want {
		t.Fatalf("upper AppRoot() = %q, want %q", got, want)
	}
	if got, want := lower.AppRoot(), filepath.Join(base, "auto-mas"); got != want {
		t.Fatalf("lower AppRoot() = %q, want %q", got, want)
	}
}

func TestNewLayout_DoesNotTouchFilesystem(t *testing.T) {
	base := t.TempDir()
	appRoot := filepath.Join(base, "does-not-exist")
	if _, err := os.Stat(appRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want errors.Is(_, fs.ErrNotExist)", appRoot, err)
	}

	if _, err := config.NewLayout(appRoot, base); err != nil {
		t.Fatalf("NewLayout(%q, %q) error = %v", appRoot, base, err)
	}
	if _, err := os.Stat(appRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) after NewLayout error = %v, want errors.Is(_, fs.ErrNotExist)", appRoot, err)
	}
}
