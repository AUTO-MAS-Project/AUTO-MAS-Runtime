package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMacOSWorkflow_DetachRetriesWithoutForce(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("missing test source path")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "macos-prototype.yml"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, required := range []string{"$attempt -le 5", "hdiutil detach $mountPath", "lsof +D $mountPath", "throw 'APFSX detach failed after 5 attempts'"} {
		if !strings.Contains(source, required) {
			t.Errorf("missing detach behavior %q", required)
		}
	}
	if strings.Contains(source, "hdiutil detach -force") || strings.Contains(source, "hdiutil detach $mountPath -force") {
		t.Fatal("detach must not force unmount")
	}
}
