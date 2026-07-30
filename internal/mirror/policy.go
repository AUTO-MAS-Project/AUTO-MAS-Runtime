package mirror

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	errInvalidPolicy = errors.New("mirror policy is invalid")
	errInvalidPlan   = errors.New("mirror plan is invalid")
)

// PolicySpec 是 CLI 已解析镜像策略的构造输入。
type PolicySpec struct {
	Preferred  map[Kind]string
	Offline    bool
	MirrorOnly bool
}

// Policy 保存不可变的已解析镜像策略。
type Policy struct {
	preferred  map[Kind]string
	offline    bool
	mirrorOnly bool
}

// NewPolicy 校验并防御性复制 CLI 已解析值。
func NewPolicy(spec PolicySpec) (Policy, error) {
	if spec.Offline && (len(spec.Preferred) != 0 || spec.MirrorOnly) {
		return Policy{}, fmt.Errorf("%w: offline conflict", errInvalidPolicy)
	}
	preferred := make(map[Kind]string, len(spec.Preferred))
	for kind, key := range spec.Preferred {
		if !kind.Valid() || !validSourceKey(key) {
			return Policy{}, fmt.Errorf("%w: preferred source", errInvalidPolicy)
		}
		preferred[kind] = key
	}
	return Policy{
		preferred:  preferred,
		offline:    spec.Offline,
		mirrorOnly: spec.MirrorOnly,
	}, nil
}

// Preferred 返回 kind 的显式首选 key。
func (p Policy) Preferred(kind Kind) (string, bool) {
	if p.preferred == nil {
		return "", false
	}
	key, ok := p.preferred[kind]
	return key, ok
}

// Offline 报告策略是否禁止全部网络尝试。
func (p Policy) Offline() bool {
	return p.offline
}

// MirrorOnly 报告策略是否排除 official 兜底。
func (p Policy) MirrorOnly() bool {
	return p.mirrorOnly
}

// Plan 是单次操作的不可变 Source 顺序快照。
type Plan struct {
	kind    Kind
	sources []Source
	offline bool
	seal    [sha256.Size]byte
}

// BuildPlan 校验 Catalog/Policy 并生成确定性尝试顺序。
func BuildPlan(
	catalog *Catalog,
	policy Policy,
	kind Kind,
) (Plan, error) {
	if !kind.Valid() || !validPolicy(policy) || validateCatalog(catalog) != nil {
		return Plan{}, fmt.Errorf("%w: input", errInvalidPlan)
	}
	if policy.offline {
		plan := Plan{kind: kind, offline: true}
		plan.seal = planSeal(plan.kind, plan.sources, plan.offline)
		return plan, nil
	}
	for preferredKind, key := range policy.preferred {
		source, ok := catalog.Source(preferredKind, key)
		if !ok || (policy.mirrorOnly && source.official) {
			return Plan{}, fmt.Errorf("%w: preferred source", errInvalidPlan)
		}
	}

	ordered := make([]Source, 0, len(catalog.sources[kind]))
	preferredKey, hasPreferred := policy.preferred[kind]
	if hasPreferred {
		source, _ := catalog.Source(kind, preferredKey)
		ordered = append(ordered, source)
	}
	for _, source := range catalog.sources[kind] {
		if source.official || (hasPreferred && source.key == preferredKey) {
			continue
		}
		ordered = append(ordered, source)
	}
	if !policy.mirrorOnly {
		for _, source := range catalog.sources[kind] {
			if !source.official || (hasPreferred && source.key == preferredKey) {
				continue
			}
			ordered = append(ordered, source)
		}
	}
	if len(ordered) == 0 {
		return Plan{}, fmt.Errorf("%w: online plan has no source", errInvalidPlan)
	}
	plan := Plan{
		kind:    kind,
		sources: append([]Source(nil), ordered...),
		offline: false,
	}
	plan.seal = planSeal(plan.kind, plan.sources, plan.offline)
	return plan, nil
}

// Sources 返回 Plan 内有序 Source 的防御性副本。
func (p Plan) Sources() []Source {
	return append([]Source(nil), p.sources...)
}

// Kind 返回 Plan 的单一 Source 类别。
func (p Plan) Kind() Kind {
	return p.kind
}

// Offline 报告 Plan 是否为合法离线快照。
func (p Plan) Offline() bool {
	return p.offline
}

func validPolicy(policy Policy) bool {
	if policy.preferred == nil {
		return false
	}
	if policy.offline && (len(policy.preferred) != 0 || policy.mirrorOnly) {
		return false
	}
	for kind, key := range policy.preferred {
		if !kind.Valid() || !validSourceKey(key) {
			return false
		}
	}
	return true
}

func validateCatalog(catalog *Catalog) error {
	if catalog == nil || catalog.sources == nil {
		return errInvalidCatalog
	}
	for _, kind := range AllKinds() {
		sources := catalog.sources[kind]
		if len(sources) == 0 {
			return errInvalidCatalog
		}
		keys := make(map[string]struct{}, len(sources))
		officialCount := 0
		for _, source := range sources {
			if err := validateSource(source); err != nil || source.kind != kind {
				return errInvalidCatalog
			}
			if _, exists := keys[source.key]; exists {
				return errInvalidCatalog
			}
			keys[source.key] = struct{}{}
			if source.official {
				officialCount++
			}
		}
		if officialCount != 1 {
			return errInvalidCatalog
		}
	}
	return nil
}

func validPlan(plan Plan) bool {
	if !plan.kind.Valid() ||
		plan.seal != planSeal(plan.kind, plan.sources, plan.offline) {
		return false
	}
	if plan.offline {
		return len(plan.sources) == 0
	}
	if len(plan.sources) == 0 {
		return false
	}
	keys := make(map[string]struct{}, len(plan.sources))
	for _, source := range plan.sources {
		if source.kind != plan.kind || validateSource(source) != nil {
			return false
		}
		if _, exists := keys[source.key]; exists {
			return false
		}
		keys[source.key] = struct{}{}
	}
	return true
}

func planSeal(kind Kind, sources []Source, offline bool) [sha256.Size]byte {
	var builder strings.Builder
	builder.WriteString(kind.String())
	builder.WriteByte(0)
	builder.WriteString(strconv.FormatBool(offline))
	for _, source := range sources {
		builder.WriteByte(0)
		builder.WriteString(source.kind.String())
		builder.WriteByte(0)
		builder.WriteString(source.key)
		builder.WriteByte(0)
		builder.WriteString(source.baseURL)
		builder.WriteByte(0)
		builder.WriteString(strconv.FormatBool(source.official))
	}
	return sha256.Sum256([]byte(builder.String()))
}
