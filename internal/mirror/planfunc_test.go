package mirror

import (
	"context"
	"reflect"
	"testing"
)

// TestCatalogPlanFunc_MatchesBuildPlan 锁定默认 PlanFunc 与 BuildPlan 完全等价，
// 并在 ctx 已取消时不做任何事直接返回。
func TestCatalogPlanFunc_MatchesBuildPlan(t *testing.T) {
	t.Parallel()

	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	policy, err := NewPolicy(PolicySpec{Preferred: map[Kind]string{KindUV: "github"}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	plan := CatalogPlanFunc(catalog)
	for _, kind := range AllKinds() {
		want, err := BuildPlan(catalog, policy, kind)
		if err != nil {
			t.Fatalf("BuildPlan(%s) error = %v", kind, err)
		}
		got, err := plan(context.Background(), policy, kind)
		if err != nil {
			t.Fatalf("CatalogPlanFunc(%s) error = %v", kind, err)
		}
		if !reflect.DeepEqual(got.Sources(), want.Sources()) {
			t.Errorf("CatalogPlanFunc(%s) = %v, want %v", kind, got.Sources(), want.Sources())
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plan(cancelled, policy, KindUV); err == nil {
		t.Fatal("CatalogPlanFunc(cancelled ctx) error = nil, want context error")
	}
	if CatalogPlanFunc(nil) != nil {
		t.Fatal("CatalogPlanFunc(nil) != nil, want nil")
	}
}
