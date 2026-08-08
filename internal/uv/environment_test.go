package uv

import (
	"context"
	"reflect"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

func TestEnvironmentEnsure_IsIdempotent(t *testing.T) {
	steps := &environmentStepLog{}
	service, err := NewEnvironmentService(
		&fakeEnvironmentUV{steps: steps},
		&fakeEnvironmentPython{steps: steps},
		&fakeEnvironmentDependencies{steps: steps},
	)
	if err != nil {
		t.Fatalf("NewEnvironmentService() error = %v", err)
	}
	request := EnvironmentRequest{OperationID: testOperationID}
	first, err := service.Ensure(context.Background(), request)
	if err != nil {
		t.Fatalf("first Ensure() error = %v", err)
	}
	second, err := service.Ensure(context.Background(), request)
	if err != nil {
		t.Fatalf("second Ensure() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Ensure() results differ: first=%#v second=%#v", first, second)
	}
	if got, want := steps.ensureCalls, 2; got != want {
		t.Fatalf("uv ensure calls = %d, want %d", got, want)
	}
	if got, want := steps.prepareCalls, 2; got != want {
		t.Fatalf("Python prepare calls = %d, want %d", got, want)
	}
	if got, want := steps.syncCalls, 2; got != want {
		t.Fatalf("dependency sync calls = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(steps.order, []string{
		"uv.ensure", "python.prepare", "dependencies.sync",
		"uv.ensure", "python.prepare", "dependencies.sync",
	}) {
		t.Fatalf("Ensure() order = %#v", steps.order)
	}
}

func TestEnvironmentCheck_IsReadOnly(t *testing.T) {
	steps := &environmentStepLog{}
	service, err := NewEnvironmentService(
		&fakeEnvironmentUV{steps: steps},
		&fakeEnvironmentPython{steps: steps},
		&fakeEnvironmentDependencies{steps: steps},
	)
	if err != nil {
		t.Fatalf("NewEnvironmentService() error = %v", err)
	}
	result, err := service.Check(context.Background(), EnvironmentRequest{OperationID: testOperationID})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.UVReady || result.Python.Version.String() != "3.12.10" || !result.Dependencies.LockfileChecked {
		t.Fatalf("Check() result = %#v, want ready facts", result)
	}
	if got, want := steps.order, []string{"uv.check", "python.check", "dependencies.check"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Check() order = %#v, want %#v", got, want)
	}
	if steps.ensureCalls != 0 || steps.prepareCalls != 0 || steps.syncCalls != 0 {
		t.Fatalf("Check() invoked mutating steps: %#v", steps)
	}
}

type environmentStepLog struct {
	order        []string
	ensureCalls  int
	prepareCalls int
	syncCalls    int
}

type fakeEnvironmentUV struct{ steps *environmentStepLog }

func (s *fakeEnvironmentUV) Ensure(context.Context, string, mirror.Policy) (string, error) {
	s.steps.order = append(s.steps.order, "uv.ensure")
	s.steps.ensureCalls++
	return "runtime/tools/uv/0.12.3/uv.exe", nil
}

func (s *fakeEnvironmentUV) Check(context.Context) (bool, error) {
	s.steps.order = append(s.steps.order, "uv.check")
	return true, nil
}

type fakeEnvironmentPython struct{ steps *environmentStepLog }

func (s *fakeEnvironmentPython) Prepare(context.Context, PythonRequest) (PythonResult, error) {
	s.steps.order = append(s.steps.order, "python.prepare")
	s.steps.prepareCalls++
	return PythonResult{Spec: PythonSpec{Version: PythonVersion{Major: 3, Minor: 12, Patch: 10}}}, nil
}

func (s *fakeEnvironmentPython) Check(context.Context, PythonRequest) (PythonCheckResult, error) {
	s.steps.order = append(s.steps.order, "python.check")
	return PythonCheckResult{Spec: PythonSpec{Version: PythonVersion{Major: 3, Minor: 12, Patch: 10}}}, nil
}

type fakeEnvironmentDependencies struct{ steps *environmentStepLog }

func (s *fakeEnvironmentDependencies) Sync(context.Context, DependenciesRequest) (DependenciesResult, error) {
	s.steps.order = append(s.steps.order, "dependencies.sync")
	s.steps.syncCalls++
	return DependenciesResult{LockfileChecked: true, Synchronized: true}, nil
}

func (s *fakeEnvironmentDependencies) Check(context.Context, DependenciesRequest) (DependenciesResult, error) {
	s.steps.order = append(s.steps.order, "dependencies.check")
	return DependenciesResult{LockfileChecked: true}, nil
}

func (s *fakeEnvironmentDependencies) Rebuild(context.Context, DependenciesRequest) (DependenciesResult, error) {
	s.steps.order = append(s.steps.order, "dependencies.rebuild")
	return DependenciesResult{Rebuilt: true}, nil
}
