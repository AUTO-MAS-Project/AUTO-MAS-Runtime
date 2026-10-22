package uv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDependencies_InvalidLockAndCheckFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		lock string
		exit int
		want protocol.Code
	}{
		{name: "syntax", lock: "version = 1revision = 3", want: protocol.CodeLockfileInvalid},
		{name: "cache_miss", lock: "version = 1\n", exit: 1, want: protocol.CodeLockfileCheckFailed},
		{name: "execution", lock: "version = 1\n", exit: 2, want: protocol.CodeLockfileCheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatal(err)
			}
			writeLockfile(t, layout.UVLockFile())
			if err := os.WriteFile(layout.UVLockFile(), []byte(test.lock), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := &fakeDependenciesRunner{responses: []fakeRunnerResponse{{result: UVResult{ExitCode: test.exit}}}}
			service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Check(t.Context(), dependencyTestRequest(layout))
			assertPythonCode(t, err, test.want)
		})
	}
}
