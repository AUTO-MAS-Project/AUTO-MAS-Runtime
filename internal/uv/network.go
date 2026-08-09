package uv

import (
	"context"
	"errors"
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	uvOfflineEnv             = "UV_OFFLINE"
	uvPythonInstallMirrorEnv = "UV_PYTHON_INSTALL_MIRROR"
)

type sourceRotator interface {
	Run(
		ctx context.Context,
		plan mirror.Plan,
		target mirror.Target,
		attempt mirror.AttemptFunc,
	) (mirror.RotationResult, error)
}

// networkExecutor 把 uv 的 Python/包索引网络调用绑定到冻结的 mirror Plan。
type networkExecutor struct {
	catalog *mirror.Catalog
	rotator sourceRotator
}

func newDefaultNetworkExecutor() (*networkExecutor, error) {
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return nil, fmt.Errorf("build uv mirror catalog: %w", err)
	}
	rotator, err := mirror.NewRotator()
	if err != nil {
		return nil, fmt.Errorf("build uv mirror rotator: %w", err)
	}
	return newNetworkExecutor(catalog, rotator)
}

func newNetworkExecutor(catalog *mirror.Catalog, rotator sourceRotator) (*networkExecutor, error) {
	if catalog == nil || rotator == nil {
		return nil, errors.New("uv network dependencies are incomplete")
	}
	return &networkExecutor{catalog: catalog, rotator: rotator}, nil
}

func (e *networkExecutor) run(
	ctx context.Context,
	runner Runner,
	policy mirror.Policy,
	kind mirror.Kind,
	target mirror.Target,
	args []string,
	options RunOptions,
) (UVResult, error) {
	if ctx == nil || e == nil || e.catalog == nil || e.rotator == nil || runner == nil ||
		!kind.Valid() || target.ValidateForKind(kind) != nil || len(args) == 0 {
		return UVResult{}, errors.New("uv network request is invalid")
	}
	plan, err := e.plan(policy, kind)
	if err != nil {
		return UVResult{}, err
	}
	if plan.Offline() {
		offlineOptions := cloneRunOptions(options)
		offlineOptions.Environment[uvOfflineEnv] = "1"
		result, runErr := runner.Run(ctx, append([]string(nil), args...), offlineOptions)
		if runErr == nil && result.ExitCode == 0 {
			return result, nil
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return result, runErr
		}
		return result, newError(
			protocol.CodeNetworkUnavailable,
			options.Stage,
			"离线缓存不足，操作需要网络",
			map[string]any{"sourceKind": kind.String(), "exitCode": result.ExitCode},
			nonNilRunError(runErr),
		)
	}

	var lastResult UVResult
	_, err = e.rotator.Run(ctx, plan, target, func(attemptCtx context.Context, attempt mirror.Attempt) mirror.AttemptOutcome {
		attemptArgs := append([]string(nil), args...)
		attemptOptions := cloneRunOptions(options)
		switch kind {
		case mirror.KindPython:
			attemptOptions.Environment[uvPythonInstallMirrorEnv] = attempt.Source.BaseURL()
		case mirror.KindPackageIndex:
			attemptArgs = append(attemptArgs, "--default-index", attempt.Source.BaseURL())
		default:
			return mirror.AttemptOutcome{
				Kind:        mirror.OutcomeTargetFailure,
				FailureKind: mirror.FailureKind("unsupported_kind"),
				Err:         errors.New("uv network kind is unsupported"),
			}
		}
		result, runErr := runner.Run(attemptCtx, attemptArgs, attemptOptions)
		lastResult = result
		if runErr == nil && result.ExitCode == 0 {
			return mirror.AttemptOutcome{Kind: mirror.OutcomeSucceeded}
		}
		return mirror.AttemptOutcome{
			Kind:        mirror.OutcomeRetrySameSource,
			FailureKind: mirror.FailureKind("uv_exec"),
			Err:         nonNilRunError(runErr),
		}
	})
	if err == nil {
		return lastResult, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return lastResult, err
	}
	var rotationErr *mirror.RotationError
	if errors.As(err, &rotationErr) {
		code := rotationErr.Code()
		message := "网络源不可用"
		if code == protocol.CodeMirrorExhausted {
			message = "所有镜像源均不可用"
		}
		return lastResult, newError(
			code,
			options.Stage,
			message,
			map[string]any{"sourceKind": kind.String(), "exitCode": lastResult.ExitCode},
			err,
		)
	}
	return lastResult, err
}

func (e *networkExecutor) plan(policy mirror.Policy, kind mirror.Kind) (mirror.Plan, error) {
	plan, err := mirror.BuildPlan(e.catalog, policy, kind)
	if err == nil {
		return plan, nil
	}
	defaultPolicy, defaultErr := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}})
	if defaultErr != nil {
		return mirror.Plan{}, fmt.Errorf("build default uv mirror policy: %w", defaultErr)
	}
	plan, defaultErr = mirror.BuildPlan(e.catalog, defaultPolicy, kind)
	if defaultErr != nil {
		return mirror.Plan{}, fmt.Errorf("build uv mirror plan: %w", errors.Join(err, defaultErr))
	}
	return plan, nil
}

func cloneRunOptions(options RunOptions) RunOptions {
	options.Environment = cloneEnvironment(options.Environment)
	if options.Environment == nil {
		options.Environment = make(map[string]string)
	}
	return options
}

func nonNilRunError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("uv exited with a non-zero status")
}

func isNetworkPolicyError(err error) bool {
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		return false
	}
	return operationErr.Code() == protocol.CodeNetworkUnavailable ||
		operationErr.Code() == protocol.CodeMirrorExhausted
}
