package gitrepo

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseTarget_ValidVersions(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "minimal", version: "v1"},
		{name: "standard", version: "v5.4.0"},
		{name: "prerelease", version: "v5.4.0-beta.4"},
		{name: "historical withplugin", version: "v5.2.0-withplugin.0.0.1"},
		{name: "allowed uppercase and underscore", version: "vRC_1-A"},
		{name: "maximum bytes", version: "v" + strings.Repeat("a", 127)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseTarget(tt.version)
			if err != nil {
				t.Fatalf("ParseTarget(%q) error = %v", tt.version, err)
			}
			if got := target.Version(); got != tt.version {
				t.Fatalf("Version() = %q, want %q", got, tt.version)
			}
			if got, want := target.Branch(), "release/"+tt.version; got != want {
				t.Fatalf("Branch() = %q, want %q", got, want)
			}
			if err := target.validate(); err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}
}

func TestParseTarget_InvalidVersions(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "empty", version: ""},
		{name: "only prefix", version: "v"},
		{name: "uppercase prefix", version: "V5.4.0"},
		{name: "missing prefix", version: "5.4.0"},
		{name: "forward slash", version: "v5/release"},
		{name: "backslash", version: `v5\release`},
		{name: "space", version: "v5 4"},
		{name: "tab", version: "v5\t4"},
		{name: "control", version: "v5\x004"},
		{name: "non ASCII", version: "v版本"},
		{name: "dot dot", version: "v5..4"},
		{name: "reflog syntax", version: "v5@{1}"},
		{name: "trailing dot", version: "v5.4."},
		{name: "lock suffix", version: "vfoo.lock"},
		{name: "git ref colon", version: "v5:4"},
		{name: "git ref tilde", version: "v5~4"},
		{name: "too many bytes", version: "v" + strings.Repeat("a", 128)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseTarget(tt.version)
			if err == nil {
				t.Fatalf("ParseTarget(%q) = %#v, nil; want error", tt.version, target)
			}
			if !errors.Is(err, ErrInvalidVersion) {
				t.Fatalf("ParseTarget(%q) error = %v, want ErrInvalidVersion", tt.version, err)
			}
		})
	}
}

func TestTarget_ValuesAreImmutable(t *testing.T) {
	targetType := reflect.TypeFor[Target]()
	for i := 0; i < targetType.NumField(); i++ {
		field := targetType.Field(i)
		if field.IsExported() {
			t.Fatalf("Target field %q is exported", field.Name)
		}
	}

	target, err := ParseTarget("v5.4.0-beta.4")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	versionCopy := []byte(target.Version())
	branchCopy := []byte(target.Branch())
	versionCopy[1] = '0'
	branchCopy[len(branchCopy)-1] = '0'
	if string(versionCopy) == target.Version() || string(branchCopy) == target.Branch() {
		t.Fatal("mutated copies unexpectedly equal Target values")
	}
	if got := target.Version(); got != "v5.4.0-beta.4" {
		t.Fatalf("Version() after returned value changed = %q, want original", got)
	}
	if got := target.Branch(); got != "release/v5.4.0-beta.4" {
		t.Fatalf("Branch() after returned value changed = %q, want original", got)
	}

	invalidTargets := []Target{
		{},
		{version: "v5.4.0", branch: "release/v0"},
		{version: "v5/4", branch: "release/v5/4"},
	}
	for _, invalid := range invalidTargets {
		if err := invalid.validate(); err == nil {
			t.Fatalf("Target %#v validate() = nil, want error", invalid)
		}
	}
}
