package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// parseTestOptions 直接调用未导出的 parseGlobalOptions，绕开 Execute 会话。
func parseTestOptions(t *testing.T, cwd string, args ...string) (globalOptions, error) {
	t.Helper()
	deps := &deps{
		options: options{
			cwd:   cwd,
			clock: time.Now,
		},
	}
	root := newRoot(deps)
	target, remaining, err := root.Find(args)
	if err != nil {
		return globalOptions{}, err
	}
	if err := target.ParseFlags(remaining); err != nil {
		return globalOptions{}, err
	}
	return parseGlobalOptions(target.Flags(), deps.options.cwd)
}

func TestParseGlobalOptions_DefaultOutputHuman(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(t, t.TempDir(), "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if opts.output != outputHuman {
		t.Errorf("output = %q, want human", opts.output)
	}
}

func TestParseGlobalOptions_NDJSONOutput(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(t, t.TempDir(), "--output", "ndjson", "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if opts.output != outputNDJSON {
		t.Errorf("output = %q, want ndjson", opts.output)
	}
}

func TestParseGlobalOptions_InvalidOutput(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--output", "yaml", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want invalid output error")
	}
}

func TestParseGlobalOptions_AppRootRelativeResolvesUnderCWD(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	opts, err := parseTestOptions(t, cwd, "--app-root", "sub", "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if opts.layout.AppRoot() == "" || !strings.Contains(opts.layout.AppRoot(), "sub") {
		t.Errorf("app root = %q, want resolved under %q", opts.layout.AppRoot(), cwd)
	}
}

func TestParseGlobalOptions_AppRootVolumeRootRejected(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--app-root", `C:\`, "doctor")
	if err == nil || !errors.Is(err, config.ErrAppRootIsRoot) {
		t.Fatalf("parseGlobalOptions() error = %v, want ErrAppRootIsRoot", err)
	}
}

func TestParseGlobalOptions_ProtocolDefaultIsCurrent(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(t, t.TempDir(), "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if opts.protocolVersion != protocol.Version {
		t.Errorf("protocol = %d, want %d", opts.protocolVersion, protocol.Version)
	}
}

func TestParseGlobalOptions_ProtocolMismatch(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--protocol", "2", "doctor")
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("parseGlobalOptions() error = %v, want errProtocolMismatch", err)
	}
}

func TestParseGlobalOptions_OfflineAloneValid(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(t, t.TempDir(), "--offline", "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if !opts.mirrorPolicy.Offline() {
		t.Error("mirror policy offline = false, want true")
	}
}

func TestParseGlobalOptions_OfflineWithMirrorConflict(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--offline", "--mirror", "git=cnb", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want offline conflict")
	}
}

func TestParseGlobalOptions_OfflineWithMirrorOnlyConflict(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--offline", "--mirror-only", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want offline conflict")
	}
}

func TestParseGlobalOptions_MirrorMultipleKindsPreserved(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(
		t,
		t.TempDir(),
		"--mirror", "git=cnb",
		"--mirror", "uv=gh-proxy",
		"--mirror", "python=gh-proxy",
		"--mirror", "package-index=aliyun",
		"doctor",
	)
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	for _, test := range []struct {
		kind mirror.Kind
		want string
	}{
		{kind: mirror.KindGit, want: "cnb"},
		{kind: mirror.KindUV, want: "gh-proxy"},
		{kind: mirror.KindPython, want: "gh-proxy"},
		{kind: mirror.KindPackageIndex, want: "aliyun"},
	} {
		got, ok := opts.mirrorPolicy.Preferred(test.kind)
		if !ok || got != test.want {
			t.Errorf("Preferred(%s) = %q, %t; want %q, true", test.kind, got, ok, test.want)
		}
	}
	if opts.mirrorPolicy.Offline() || opts.mirrorPolicy.MirrorOnly() {
		t.Errorf("policy offline=%t mirrorOnly=%t, want false/false", opts.mirrorPolicy.Offline(), opts.mirrorPolicy.MirrorOnly())
	}
}

func TestParseGlobalOptions_MirrorDuplicateKindRejected(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--mirror", "git=cnb", "--mirror", "git=github", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want duplicate kind error")
	}
}

func TestParseGlobalOptions_MirrorInvalidKindRejected(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--mirror", "ftp=github", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want invalid kind error")
	}
}

func TestParseGlobalOptions_MirrorInvalidKeyRejected(t *testing.T) {
	t.Parallel()
	_, err := parseTestOptions(t, t.TempDir(), "--mirror", "git=Github!", "doctor")
	if err == nil {
		t.Fatal("parseGlobalOptions() error = nil, want invalid key error")
	}
}

func TestParseGlobalOptions_MirrorOnlyValid(t *testing.T) {
	t.Parallel()
	opts, err := parseTestOptions(t, t.TempDir(), "--mirror-only", "--mirror", "git=cnb", "doctor")
	if err != nil {
		t.Fatalf("parseGlobalOptions() error = %v", err)
	}
	if !opts.mirrorPolicy.MirrorOnly() {
		t.Error("mirror policy mirrorOnly = false, want true")
	}
}
