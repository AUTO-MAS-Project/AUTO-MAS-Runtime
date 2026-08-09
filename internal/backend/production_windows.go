//go:build windows

package backend

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// NewProductionManagedSupervisor 创建延迟打开受管依赖的 Windows supervisor。
func NewProductionManagedSupervisor(
	ctx context.Context,
	layout *config.Layout,
	stderr io.Writer,
	clock func() time.Time,
) (*ManagedSupervisor, error) {
	if ctx == nil || layout == nil || stderr == nil {
		return nil, errors.New("production backend arguments are invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	uvExecutable, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		return nil, err
	}
	runner, err := uv.NewRunner(uv.RunnerConfig{
		Executable:       uvExecutable,
		ProjectDir:       layout.RepoDir(),
		PythonInstallDir: layout.PythonDir(),
		ProjectEnvDir:    layout.VenvDir(),
		CacheDir:         layout.UVCacheDir(),
		Clock:            clock,
	})
	if err != nil {
		return nil, err
	}
	repository, err := gitrepo.NewService(layout)
	if err != nil {
		return nil, err
	}
	stateStore := &lazyStateStore{layout: layout, clock: clock}
	deps := Dependencies{
		Lock:       newLazyLockSet(layout),
		State:      stateStore,
		Repository: productionRepository{service: repository},
		Entry:      productionEntryChecker{layout: layout},
		UV:         productionUV{runner: runner, expected: uv.FixedVersion},
		Health:     health.NewChecker(health.Config{}),
		Logger: func(logCtx context.Context, request Request) (Logger, error) {
			logger, loggerErr := logging.New(
				logCtx,
				layout,
				stderr,
				"backend supervise",
				request.OperationID,
				logging.WithClock(clock),
			)
			if loggerErr != nil {
				return nil, loggerErr
			}
			return productionLogger{logger: logger}, nil
		},
		Clock:       clock,
		UVPath:      uvExecutable,
		PythonPaths: []string{layout.VenvPythonExecutable(), layout.PythonExecutable()},
		PID:         state.NewSystemPIDProbe(),
	}
	return NewManagedSupervisor(layout, deps)
}
