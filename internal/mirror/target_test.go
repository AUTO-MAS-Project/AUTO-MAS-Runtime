package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func TestNewTarget_NormalizesFieldsAndFingerprint(t *testing.T) {
	digest := strings.Repeat("A1", 32)
	baseSpec := TargetSpec{
		ProductVersion: "v5.3.0",
		ReleaseBranch:  "release/v5.3.0",
		UVVersion:      "0.8.12",
		PythonVersion:  "3.12.10",
		LockDigest:     digest,
	}
	target, err := NewTarget(baseSpec)
	if err != nil {
		t.Fatalf("NewTarget() error = %v", err)
	}
	if target.ProductVersion() != "v5.3.0" ||
		target.ReleaseBranch() != "release/v5.3.0" ||
		target.UVVersion() != "0.8.12" ||
		target.PythonVersion() != "3.12.10" ||
		target.LockDigest() != strings.ToLower(digest) {
		t.Fatalf("Target getters = %#v", target)
	}
	if len(target.Fingerprint()) != 64 {
		t.Fatalf("Fingerprint() length = %d, want 64", len(target.Fingerprint()))
	}
	t.Run("canonical golden", func(t *testing.T) {
		const wantFingerprint = "4b19e6e166df651deae4858e4f0936b2301c29b65f58e40f281e3e22600907fb"
		if target.Fingerprint() != wantFingerprint {
			t.Fatalf("Fingerprint() = %q, want %q", target.Fingerprint(), wantFingerprint)
		}
	})
	t.Run("valid SHA-256 hex", func(t *testing.T) {
		decoded, err := hex.DecodeString(target.Fingerprint())
		if err != nil {
			t.Fatalf("hex.DecodeString(Fingerprint()) error = %v", err)
		}
		if len(decoded) != sha256.Size {
			t.Fatalf("decoded fingerprint length = %d, want %d", len(decoded), sha256.Size)
		}
	})
	same, err := NewTarget(TargetSpec{
		ProductVersion: "v5.3.0",
		ReleaseBranch:  "release/v5.3.0",
		UVVersion:      "0.8.12",
		PythonVersion:  "3.12.10",
		LockDigest:     strings.ToLower(digest),
	})
	if err != nil {
		t.Fatalf("NewTarget(same) error = %v", err)
	}
	if same.Fingerprint() != target.Fingerprint() {
		t.Fatalf("equivalent fingerprints = %q/%q", same.Fingerprint(), target.Fingerprint())
	}

	tests := []struct {
		name   string
		mutate func(*TargetSpec)
	}{
		{
			name: "product version",
			mutate: func(spec *TargetSpec) {
				spec.ProductVersion = "v5.3.1"
			},
		},
		{
			name: "release branch",
			mutate: func(spec *TargetSpec) {
				spec.ReleaseBranch = "release/v5.3.1"
			},
		},
		{
			name: "uv version",
			mutate: func(spec *TargetSpec) {
				spec.UVVersion = "0.8.13"
			},
		},
		{
			name: "python version",
			mutate: func(spec *TargetSpec) {
				spec.PythonVersion = "3.12.11"
			},
		},
		{
			name: "lock digest",
			mutate: func(spec *TargetSpec) {
				spec.LockDigest = strings.Repeat("b2", 32)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedSpec := baseSpec
			test.mutate(&changedSpec)
			changed, err := NewTarget(changedSpec)
			if err != nil {
				t.Fatalf("NewTarget(changed) error = %v", err)
			}
			if changed.Fingerprint() == target.Fingerprint() {
				t.Fatalf(
					"Fingerprint() = %q after changing %s, want different value",
					changed.Fingerprint(),
					test.name,
				)
			}
		})
	}
}

func TestNewTarget_RejectsUnsafeFields(t *testing.T) {
	tests := []struct {
		name string
		spec TargetSpec
	}{
		{name: "empty", spec: TargetSpec{}},
		{name: "product missing v", spec: TargetSpec{ProductVersion: "5.3.0"}},
		{name: "product slash", spec: TargetSpec{ProductVersion: "v5/3"}},
		{name: "product dot dot", spec: TargetSpec{ProductVersion: "v5..3"}},
		{name: "product reflog", spec: TargetSpec{ProductVersion: "v5@{1}"}},
		{name: "product trailing dot", spec: TargetSpec{ProductVersion: "v5.3."}},
		{name: "product control", spec: TargetSpec{ProductVersion: "v5\n3"}},
		{name: "product too long", spec: TargetSpec{ProductVersion: "v" + strings.Repeat("a", 128)}},
		{name: "branch prefix", spec: TargetSpec{ReleaseBranch: "main"}},
		{name: "branch unsafe suffix", spec: TargetSpec{ReleaseBranch: "release/v5/3"}},
		{
			name: "branch suffix too long",
			spec: TargetSpec{
				ReleaseBranch: "release/v" + strings.Repeat("a", 128),
			},
		},
		{name: "uv unsafe", spec: TargetSpec{UVVersion: "0.8 beta"}},
		{name: "python unsafe", spec: TargetSpec{PythonVersion: `3.12\10`}},
		{name: "digest short", spec: TargetSpec{LockDigest: "abcd"}},
		{name: "digest non hex", spec: TargetSpec{LockDigest: strings.Repeat("z", 64)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewTarget(test.spec); err == nil {
				t.Fatal("NewTarget() error = nil, want validation error")
			}
		})
	}
}

func TestTarget_ValidateForKind(t *testing.T) {
	validDigest := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		kind    Kind
		spec    TargetSpec
		wantErr bool
	}{
		{
			name: KindGit.String(),
			kind: KindGit,
			spec: TargetSpec{
				ProductVersion: "v5.3.0",
				ReleaseBranch:  "release/v5.3.0",
			},
		},
		{
			name: "git maximum product version",
			kind: KindGit,
			spec: TargetSpec{
				ProductVersion: "v" + strings.Repeat("a", 127),
				ReleaseBranch:  "release/v" + strings.Repeat("a", 127),
			},
		},
		{
			name:    "git missing branch",
			kind:    KindGit,
			spec:    TargetSpec{ProductVersion: "v5.3.0"},
			wantErr: true,
		},
		{
			name: "git mismatched branch",
			kind: KindGit,
			spec: TargetSpec{
				ProductVersion: "v5.3.0",
				ReleaseBranch:  "release/v5.4.0",
			},
			wantErr: true,
		},
		{
			name: KindUV.String(),
			kind: KindUV,
			spec: TargetSpec{UVVersion: "0.8.12"},
		},
		{
			name:    "uv missing",
			kind:    KindUV,
			spec:    TargetSpec{PythonVersion: "3.12.10"},
			wantErr: true,
		},
		{
			name: KindPython.String(),
			kind: KindPython,
			spec: TargetSpec{PythonVersion: "3.12.10"},
		},
		{
			name:    "python missing",
			kind:    KindPython,
			spec:    TargetSpec{UVVersion: "0.8.12"},
			wantErr: true,
		},
		{
			name: KindPackageIndex.String(),
			kind: KindPackageIndex,
			spec: TargetSpec{LockDigest: validDigest},
		},
		{
			name:    "package index missing",
			kind:    KindPackageIndex,
			spec:    TargetSpec{UVVersion: "0.8.12"},
			wantErr: true,
		},
		{
			name:    "unknown kind",
			kind:    Kind("unknown"),
			spec:    TargetSpec{UVVersion: "0.8.12"},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := NewTarget(test.spec)
			if err != nil {
				t.Fatalf("NewTarget() error = %v", err)
			}
			err = target.ValidateForKind(test.kind)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateForKind(%q) error = %v, wantErr %t", test.kind, err, test.wantErr)
			}
		})
	}
}

func TestTarget_RejectsForgedFingerprint(t *testing.T) {
	t.Run("field changed without matching fingerprint", func(t *testing.T) {
		target, err := NewTarget(TargetSpec{UVVersion: "0.8.12"})
		if err != nil {
			t.Fatalf("NewTarget() error = %v", err)
		}
		target.uvVersion = "0.9.0"
		if err := target.ValidateForKind(KindUV); err == nil {
			t.Fatal(
				"ValidateForKind() error = nil, want forged-target rejection",
			)
		}
	})

	t.Run("noncanonical digest with canonical fingerprint", func(t *testing.T) {
		target, err := NewTarget(TargetSpec{
			LockDigest: strings.Repeat("a1", 32),
		})
		if err != nil {
			t.Fatalf("NewTarget() error = %v", err)
		}
		target.lockDigest = strings.ToUpper(target.lockDigest)
		if err := target.ValidateForKind(KindPackageIndex); err == nil {
			t.Fatal(
				"ValidateForKind() error = nil, want noncanonical-target rejection",
			)
		}
	})
}

func TestTarget_HasNoExpectedCommitField(t *testing.T) {
	targetType := reflect.TypeOf(Target{})
	if targetType.NumField() != 6 {
		t.Fatalf("Target field count = %d, want five target fields plus fingerprint", targetType.NumField())
	}
	for i := 0; i < targetType.NumField(); i++ {
		field := targetType.Field(i)
		if field.IsExported() {
			t.Fatalf("Target field %q is exported", field.Name)
		}
		if strings.Contains(strings.ToLower(field.Name), "commit") {
			t.Fatalf("Target contains Commit field %q", field.Name)
		}
	}
}
