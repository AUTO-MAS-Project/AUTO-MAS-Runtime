package mirror

import (
	"errors"
	"reflect"
	"testing"
)

func TestNewPolicy_ValidatesAndCopiesPreferred(t *testing.T) {
	preferred := map[Kind]string{
		KindGit: "cnb",
		KindUV:  "gh-proxy",
	}
	policy, err := NewPolicy(PolicySpec{Preferred: preferred})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	preferred[KindGit] = "github"
	if got, ok := policy.Preferred(KindGit); !ok || got != "cnb" {
		t.Fatalf("Preferred(git) = %q, %t, want cnb/true", got, ok)
	}
	if policy.Offline() || policy.MirrorOnly() {
		t.Fatalf("Policy flags = offline %t/mirror-only %t", policy.Offline(), policy.MirrorOnly())
	}

	empty, err := NewPolicy(PolicySpec{})
	if err != nil {
		t.Fatalf("NewPolicy(empty) error = %v", err)
	}
	if _, ok := empty.Preferred(KindGit); ok {
		t.Fatal("Preferred(git) ok = true, want false")
	}

	tests := []struct {
		name string
		spec PolicySpec
	}{
		{
			name: "offline preferred conflict",
			spec: PolicySpec{
				Preferred: map[Kind]string{KindGit: "cnb"},
				Offline:   true,
			},
		},
		{
			name: "offline mirror-only conflict",
			spec: PolicySpec{Offline: true, MirrorOnly: true},
		},
		{
			name: "unknown kind",
			spec: PolicySpec{
				Preferred: map[Kind]string{Kind("unknown"): "source"},
			},
		},
		{
			name: "invalid key",
			spec: PolicySpec{
				Preferred: map[Kind]string{KindGit: "GitHub"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewPolicy(test.spec); err == nil {
				t.Fatal("NewPolicy() error = nil, want validation error")
			}
		})
	}
}

func TestPolicySpec_ConsumesOneParsedValuePerKind(t *testing.T) {
	field, ok := reflect.TypeOf(PolicySpec{}).FieldByName("Preferred")
	if !ok || field.Type != reflect.TypeOf(map[Kind]string{}) {
		t.Fatalf("PolicySpec.Preferred type = %v, want map[Kind]string", field.Type)
	}
}

func TestBuildPlan_DeterministicOrder(t *testing.T) {
	catalog := mustDefaultCatalog(t)
	tests := []struct {
		name    string
		kind    Kind
		spec    PolicySpec
		want    []string
		offline bool
	}{
		{
			name: "git default",
			kind: KindGit,
			want: []string{"cnb", "github"},
		},
		{
			name: "git preferred mirror",
			kind: KindGit,
			spec: PolicySpec{
				Preferred: map[Kind]string{KindGit: "cnb"},
			},
			want: []string{"cnb", "github"},
		},
		{
			name: "git preferred official",
			kind: KindGit,
			spec: PolicySpec{
				Preferred: map[Kind]string{KindGit: "github"},
			},
			want: []string{"github", "cnb"},
		},
		{
			name: "git mirror only",
			kind: KindGit,
			spec: PolicySpec{MirrorOnly: true},
			want: []string{"cnb"},
		},
		{
			name: "package index selected mirror",
			kind: KindPackageIndex,
			spec: PolicySpec{
				Preferred: map[Kind]string{KindPackageIndex: "tsinghua"},
			},
			want: []string{"tsinghua", "aliyun", "ustc", "pypi"},
		},
		{
			name:    "offline",
			kind:    KindPython,
			spec:    PolicySpec{Offline: true},
			want:    []string{},
			offline: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, err := NewPolicy(test.spec)
			if err != nil {
				t.Fatalf("NewPolicy() error = %v", err)
			}
			plan, err := BuildPlan(catalog, policy, test.kind)
			if err != nil {
				t.Fatalf("BuildPlan() error = %v", err)
			}
			if plan.Kind() != test.kind || plan.Offline() != test.offline {
				t.Fatalf("Plan = kind %q/offline %t", plan.Kind(), plan.Offline())
			}
			if got := planSourceKeys(plan); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Plan sources = %#v, want %#v", got, test.want)
			}
			seen := make(map[string]struct{})
			for _, source := range plan.Sources() {
				if _, exists := seen[source.Key()]; exists {
					t.Fatalf("Plan repeats source %q", source.Key())
				}
				seen[source.Key()] = struct{}{}
				if test.spec.MirrorOnly && source.Official() {
					t.Fatalf("mirror-only Plan contains official source %q", source.Key())
				}
			}
		})
	}
}

func TestBuildPlan_PreferredKindsRemainIndependent(t *testing.T) {
	catalog := mustDefaultCatalog(t)
	preferred := map[Kind]string{
		KindGit:          "github",
		KindUV:           "github",
		KindPython:       "github",
		KindPackageIndex: "ustc",
	}
	policy, err := NewPolicy(PolicySpec{Preferred: preferred})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	tests := []struct {
		kind Kind
		want []string
	}{
		{kind: KindGit, want: []string{"github", "cnb"}},
		{kind: KindUV, want: []string{"github", "gh-proxy"}},
		{kind: KindPython, want: []string{"github", "gh-proxy"}},
		{
			kind: KindPackageIndex,
			want: []string{"ustc", "aliyun", "tsinghua", "pypi"},
		},
	}
	for _, test := range tests {
		t.Run(test.kind.String(), func(t *testing.T) {
			planWithAll, err := BuildPlan(catalog, policy, test.kind)
			if err != nil {
				t.Fatalf("BuildPlan(all preferred) error = %v", err)
			}
			singlePolicy, err := NewPolicy(PolicySpec{
				Preferred: map[Kind]string{
					test.kind: preferred[test.kind],
				},
			})
			if err != nil {
				t.Fatalf("NewPolicy(single preferred) error = %v", err)
			}
			planWithOne, err := BuildPlan(
				catalog,
				singlePolicy,
				test.kind,
			)
			if err != nil {
				t.Fatalf("BuildPlan(single preferred) error = %v", err)
			}
			gotAll := planSourceKeys(planWithAll)
			gotOne := planSourceKeys(planWithOne)
			if !reflect.DeepEqual(gotAll, test.want) ||
				!reflect.DeepEqual(gotAll, gotOne) {
				t.Fatalf(
					"Plan sources = all %#v/single %#v, want %#v",
					gotAll,
					gotOne,
					test.want,
				)
			}
		})
	}
}

func TestBuildPlan_RejectsInvalidSelectionAndEmptyOnlinePlan(t *testing.T) {
	catalog := mustDefaultCatalog(t)
	tests := []struct {
		name         string
		catalog      *Catalog
		spec         PolicySpec
		kind         Kind
		wantRejected bool
	}{
		{
			name:    "nil catalog",
			catalog: nil,
			kind:    KindGit,
		},
		{
			name:    "zero policy",
			catalog: catalog,
			kind:    KindGit,
		},
		{
			name:    "unknown kind",
			catalog: catalog,
			spec:    PolicySpec{},
			kind:    Kind("unknown"),
		},
		{
			name:         "missing preferred",
			catalog:      catalog,
			wantRejected: true,
			spec: PolicySpec{
				Preferred: map[Kind]string{KindGit: "missing"},
			},
			kind: KindGit,
		},
		{
			name:         "official preferred in mirror-only",
			catalog:      catalog,
			wantRejected: true,
			spec: PolicySpec{
				Preferred:  map[Kind]string{KindGit: "github"},
				MirrorOnly: true,
			},
			kind: KindGit,
		},
		{
			name:         "official-only catalog in mirror-only",
			catalog:      officialOnlyGitCatalog(t),
			spec:         PolicySpec{MirrorOnly: true},
			kind:         KindGit,
			wantRejected: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var policy Policy
			if test.name != "zero policy" {
				var err error
				policy, err = NewPolicy(test.spec)
				if err != nil {
					t.Fatalf("NewPolicy() error = %v", err)
				}
			}
			_, err := BuildPlan(test.catalog, policy, test.kind)
			if err == nil {
				t.Fatal("BuildPlan() error = nil, want validation error")
			}
			if got := errors.Is(err, ErrPolicyRejected); got != test.wantRejected {
				t.Fatalf("BuildPlan() ErrPolicyRejected = %t, want %t; error=%v", got, test.wantRejected, err)
			}
		})
	}
}

func TestPlan_SourcesAreDefensiveSnapshot(t *testing.T) {
	catalog := mustDefaultCatalog(t)
	policy, err := NewPolicy(PolicySpec{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan, err := BuildPlan(catalog, policy, KindGit)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	first := plan.Sources()
	first[0] = Source{}
	if got := plan.Sources()[0].Key(); got != "cnb" {
		t.Fatalf("Plan.Sources()[0] = %q, want cnb", got)
	}
}

func mustDefaultCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	return catalog
}

func officialOnlyGitCatalog(t *testing.T) *Catalog {
	t.Helper()
	sources := allDefaultSources(t)
	filtered := make([]Source, 0, len(sources))
	for _, source := range sources {
		if source.Kind() == KindGit && !source.Official() {
			continue
		}
		filtered = append(filtered, source)
	}
	catalog, err := NewCatalog(filtered)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func planSourceKeys(plan Plan) []string {
	sources := plan.Sources()
	keys := make([]string, len(sources))
	for i, source := range sources {
		keys[i] = source.Key()
	}
	return keys
}
