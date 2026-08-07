package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var errInvalidTarget = errors.New("mirror target is invalid")

// TargetSpec 是目标值对象的构造输入。
type TargetSpec struct {
	ProductVersion string
	ReleaseBranch  string
	UVVersion      string
	PythonVersion  string
	LockDigest     string
}

// Target 保存跨 Source 尝试不可改变的目标字段。
type Target struct {
	productVersion string
	releaseBranch  string
	uvVersion      string
	pythonVersion  string
	lockDigest     string
	fingerprint    string
}

// NewTarget 校验、规范化并密封目标字段。
func NewTarget(spec TargetSpec) (Target, error) {
	normalized, err := normalizeTargetSpec(spec)
	if err != nil {
		return Target{}, err
	}
	target := Target{
		productVersion: normalized.ProductVersion,
		releaseBranch:  normalized.ReleaseBranch,
		uvVersion:      normalized.UVVersion,
		pythonVersion:  normalized.PythonVersion,
		lockDigest:     normalized.LockDigest,
	}
	target.fingerprint = targetFingerprint(normalized)
	return target, nil
}

// ProductVersion 返回目标产品版本。
func (t Target) ProductVersion() string {
	return t.productVersion
}

// ReleaseBranch 返回产品版本派生的发布分支。
func (t Target) ReleaseBranch() string {
	return t.releaseBranch
}

// UVVersion 返回目标 uv 版本。
func (t Target) UVVersion() string {
	return t.uvVersion
}

// PythonVersion 返回目标 Python 版本。
func (t Target) PythonVersion() string {
	return t.pythonVersion
}

// LockDigest 返回规范化的小写锁文件 SHA-256。
func (t Target) LockDigest() string {
	return t.lockDigest
}

// Fingerprint 返回带字段名规范序列的 SHA-256。
func (t Target) Fingerprint() string {
	return t.fingerprint
}

// ValidateForKind 校验 kind 执行所需字段与额外关系。
func (t Target) ValidateForKind(kind Kind) error {
	if !kind.Valid() || validateTarget(t) != nil {
		return fmt.Errorf("%w: invariant", errInvalidTarget)
	}
	switch kind {
	case KindGit:
		if t.productVersion == "" ||
			t.releaseBranch != "release/"+t.productVersion {
			return fmt.Errorf("%w: git target", errInvalidTarget)
		}
	case KindUV:
		if t.uvVersion == "" {
			return fmt.Errorf("%w: uv target", errInvalidTarget)
		}
	case KindPython:
		if t.pythonVersion == "" {
			return fmt.Errorf("%w: python target", errInvalidTarget)
		}
	case KindPackageIndex:
		if t.lockDigest == "" {
			return fmt.Errorf("%w: package-index target", errInvalidTarget)
		}
	}
	return nil
}

func normalizeTargetSpec(spec TargetSpec) (TargetSpec, error) {
	if spec.ProductVersion == "" &&
		spec.ReleaseBranch == "" &&
		spec.UVVersion == "" &&
		spec.PythonVersion == "" &&
		spec.LockDigest == "" {
		return TargetSpec{}, fmt.Errorf("%w: empty", errInvalidTarget)
	}
	for _, value := range []string{
		spec.ProductVersion,
		spec.UVVersion,
		spec.PythonVersion,
	} {
		if value != "" && !validVersionValue(value) {
			return TargetSpec{}, fmt.Errorf("%w: version field", errInvalidTarget)
		}
	}
	if spec.ProductVersion != "" && !strings.HasPrefix(spec.ProductVersion, "v") {
		return TargetSpec{}, fmt.Errorf("%w: product version", errInvalidTarget)
	}
	if spec.ReleaseBranch != "" {
		if !strings.HasPrefix(spec.ReleaseBranch, "release/") {
			return TargetSpec{}, fmt.Errorf("%w: release branch", errInvalidTarget)
		}
		version := strings.TrimPrefix(spec.ReleaseBranch, "release/")
		if !validVersionValue(version) || !strings.HasPrefix(version, "v") {
			return TargetSpec{}, fmt.Errorf("%w: release branch", errInvalidTarget)
		}
	}
	if spec.LockDigest != "" {
		if len(spec.LockDigest) != sha256.Size*2 {
			return TargetSpec{}, fmt.Errorf("%w: lock digest", errInvalidTarget)
		}
		if _, err := hex.DecodeString(spec.LockDigest); err != nil {
			return TargetSpec{}, fmt.Errorf("%w: lock digest", errInvalidTarget)
		}
		spec.LockDigest = strings.ToLower(spec.LockDigest)
	}
	return spec, nil
}

func validVersionValue(value string) bool {
	if len(value) == 0 || len(value) > 128 ||
		strings.Contains(value, "..") ||
		strings.Contains(value, "@{") ||
		strings.HasSuffix(value, ".") ||
		strings.HasSuffix(value, ".lock") {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' ||
			character == '-' ||
			character == '_' {
			continue
		}
		return false
	}
	return true
}

func validateTarget(target Target) error {
	spec, err := normalizeTargetSpec(TargetSpec{
		ProductVersion: target.productVersion,
		ReleaseBranch:  target.releaseBranch,
		UVVersion:      target.uvVersion,
		PythonVersion:  target.pythonVersion,
		LockDigest:     target.lockDigest,
	})
	if err != nil ||
		target.productVersion != spec.ProductVersion ||
		target.releaseBranch != spec.ReleaseBranch ||
		target.uvVersion != spec.UVVersion ||
		target.pythonVersion != spec.PythonVersion ||
		target.lockDigest != spec.LockDigest ||
		target.fingerprint == "" ||
		target.fingerprint != targetFingerprint(spec) {
		return errInvalidTarget
	}
	return nil
}

func targetFingerprint(spec TargetSpec) string {
	canonical := strings.Join([]string{
		"product-version=" + spec.ProductVersion,
		"release-branch=" + spec.ReleaseBranch,
		"uv-version=" + spec.UVVersion,
		"python-version=" + spec.PythonVersion,
		"lock-digest=" + spec.LockDigest,
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
