package uv

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

func TestDependencies_BuildDependenciesUseMirrorIndex(t *testing.T) {
	for _, test := range []struct {
		name    string
		relay   bool
		indexes []string
	}{
		{name: "relay", relay: true, indexes: []string{"http://127.0.0.1:39170/simple"}},
		{name: "relay start failure rotates mirrors", indexes: []string{
			"https://mirrors.aliyun.com/pypi/simple", "https://pypi.tuna.tsinghua.edu.cn/simple",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory := &fakeRelayFactory{session: &fakeRelaySession{baseURL: "http://127.0.0.1:39170"}}
			if !test.relay {
				factory.err = errors.New("relay unavailable")
			}
			fixture := newRelayFixture(t, factory)
			if !test.relay {
				fixture.runner.responses = []fakeRunnerResponse{
					{}, {result: UVResult{ExitCode: 1}, err: errors.New("first mirror unavailable")}, {},
				}
			}
			if _, err := fixture.service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{MirrorOnly: true})); err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			calls := fixture.runner.calls
			if got, want := len(calls), 1+len(test.indexes); got != want {
				t.Fatalf("runner calls = %d, want %d", got, want)
			}
			if slices.Contains(calls[0].args, "--default-index") {
				t.Fatalf("lock check args = %q, want no index override", calls[0].args)
			}
			for i, want := range test.indexes {
				args := calls[i+1].args
				index := slices.Index(args, "--default-index")
				if index < 0 || index+1 >= len(args) || args[index+1] != want {
					t.Errorf("sync args = %q, want --default-index %s for build dependencies", args, want)
				}
				if !slices.Contains(args, "--frozen") || slices.Contains(args, "--no-build-isolation") {
					t.Errorf("sync args = %q, want frozen sync with build isolation", args)
				}
			}
			if !reflect.DeepEqual(calls[0].args, []string{
				"lock", "--project", fixture.layout.RepoDir(), "--check",
			}) {
				t.Errorf("lock check args = %q, want unchanged original lock check", calls[0].args)
			}
			fixture.assertRepositoryUntouched(t)
			fixture.assertNoStagingLeftovers(t)
		})
	}
}
