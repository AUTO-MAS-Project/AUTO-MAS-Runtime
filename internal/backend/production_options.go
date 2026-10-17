package backend

import "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"

// SupervisorOption 配置生产监督器的可选依赖。
type SupervisorOption func(*supervisorOptions)

type supervisorOptions struct {
	ranker *mirror.Ranker
}

// WithRanker 注入本进程的测速器；nil 表示按目录顺序（调用方无需自己判空）。
func WithRanker(ranker *mirror.Ranker) SupervisorOption {
	return func(options *supervisorOptions) {
		options.ranker = ranker
	}
}
