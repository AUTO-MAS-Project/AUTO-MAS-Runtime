package mirror

import "context"

// PlanFunc 按策略给出某个 Kind 的尝试顺序。
//
// 默认实现是目录顺序（CatalogPlanFunc）；增补 2 C16 起由测速排序的实现注入，
// 消费方（uv 下载器、Python 安装、依赖同步、Git 克隆、后端注入面）统一经它取顺序，
// 不再各自直接调用 BuildPlan。ctx 供实现在必要时做有界网络探测。
type PlanFunc func(ctx context.Context, policy Policy, kind Kind) (Plan, error)

// CatalogPlanFunc 返回按目录顺序构造 Plan 的 PlanFunc；catalog 为 nil 时返回 nil。
func CatalogPlanFunc(catalog *Catalog) PlanFunc {
	if catalog == nil {
		return nil
	}
	return func(ctx context.Context, policy Policy, kind Kind) (Plan, error) {
		if ctx == nil {
			return Plan{}, errInvalidPlan
		}
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		return BuildPlan(catalog, policy, kind)
	}
}
