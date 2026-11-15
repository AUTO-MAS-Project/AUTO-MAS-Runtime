package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

const startupWorkBudget = 175 * time.Second

func (s *ManagedSupervisor) checkStartupHealth(ctx context.Context, request Request, expected health.Expectation, probe health.Probe) error {
	workCtx, cancel := context.WithTimeout(ctx, max(0, request.startupDeadline.Sub(s.deps.Clock())))
	defer cancel()
	expected.StartupDeadline = request.startupDeadline
	expected.DeferBusinessStart = request.DeferBusinessStart
	err := s.deps.Health.Check(workCtx, expected, probe)
	if workCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendHealth, "后端启动工作预算耗尽", nil, err)
	}
	return err
}

func (s *ManagedSupervisor) awaitStartupPreflight(ctx context.Context, request Request, snapshot *controlState, results <-chan controlResult) (state.EnvironmentState, state.Revision, error) {
	workCtx, cancel := context.WithTimeout(ctx, max(0, request.startupDeadline.Sub(s.deps.Clock())))
	defer cancel()
	environment, revision, err := s.awaitControlPreflight(workCtx, request, snapshot, results)
	if workCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil && (err == nil || errors.Is(err, context.DeadlineExceeded)) {
		var interrupted *controlInterruptError
		if errors.As(err, &interrupted) {
			interrupted.cause = newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendSpawn, "后端启动工作预算耗尽", nil, interrupted.cause)
		} else {
			err = newError(protocol.CodeBackendHealthTimeout, protocol.StageBackendSpawn, "后端启动工作预算耗尽", nil, err)
		}
	}
	return environment, revision, err
}

func checkDeferredStartupCapability(ctx context.Context, repo string) error {
	body, err := filesystem.ReadExternalFile(ctx, config.BackendCapabilitiesFile(repo), 4097)
	var capability struct {
		DeferredBusinessStart int `json:"deferredBusinessStart"`
	}
	if err == nil && len(body) <= 4096 {
		err = json.Unmarshal(body, &capability)
	}
	if err != nil || len(body) > 4096 || capability.DeferredBusinessStart != 1 {
		return newError(protocol.CodeUnsupportedMode, protocol.StageBackendSpawn, "后端不支持暂缓自动业务", map[string]any{"reason": "deferred_start_unsupported"}, err)
	}
	return nil
}

// HTTPActivator 的成功必须来自对应后端确认 active，不能把请求发出当作放行成功。
type HTTPActivator interface {
	Activate(context.Context, int) error
}

type loopbackHTTPActivator struct{}

func (loopbackHTTPActivator) Activate(ctx context.Context, port int) error {
	state, err := requestStartupActivation(ctx, port, http.MethodPost, "/api/core/startup/activate")
	if err != nil {
		return err
	}
	if state != "active" {
		return errors.New("backend did not confirm activation")
	}
	return nil
}

func (loopbackHTTPActivator) Status(ctx context.Context, port int) (string, error) {
	return requestStartupActivation(ctx, port, http.MethodGet, "/api/core/health")
}

func requestStartupActivation(ctx context.Context, port int, method, path string) (string, error) {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, method, health.BaseURL(port)+path, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || len(body) > 4096 {
		return "", fmt.Errorf("backend activation returned status %d", response.StatusCode)
	}
	var result struct {
		StartupActivation string `json:"startupActivation"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.StartupActivation, nil
}

func (s *ManagedSupervisor) refreshActivationStatus(ctx context.Context, request Request, snapshot *controlState) {
	_, _, details := snapshot.snapshot()
	if details["startupActivation"] != "unknown" {
		return
	}
	activator := s.deps.Activation
	if activator == nil {
		activator = loopbackHTTPActivator{}
	}
	query, ok := activator.(interface {
		Status(context.Context, int) (string, error)
	})
	if !ok {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	status, err := query.Status(queryCtx, request.Port)
	if err == nil && (status == "active" || status == "pending") {
		details["startupActivation"] = status
		snapshot.set(protocol.StageBackendRun, protocol.StateRunning, details)
	}
}

func (s *ManagedSupervisor) activateAutomaticWork(ctx context.Context, request Request, snapshot *controlState, commandID string) error {
	if !request.DeferBusinessStart {
		return nil
	}
	_, _, details := snapshot.snapshot()
	if details["startupActivation"] != "active" {
		if _, terminal := terminalCommand(request.Control); terminal {
			return nil
		}
		activator := s.deps.Activation
		if activator == nil {
			activator = loopbackHTTPActivator{}
		}
		activationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := activator.Activate(activationCtx, request.Port)
		cancel()
		if err != nil {
			return err
		}
		details["startupActivation"] = "active"
		snapshot.set(protocol.StageBackendRun, protocol.StateRunning, details)
	}
	return s.emitState(request.Emitter, protocol.StageBackendRun, protocol.StateRunning, "自动业务已放行", protocol.WithControlCommandID(details, commandID))
}
