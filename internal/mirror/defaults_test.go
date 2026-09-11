package mirror

import "testing"

// TestDefaultCatalog_PythonHasAstralBeforeGhProxy 锁定增补 2 C19：uv 自己的默认分发地址
// 必须进目录且排在 gh-proxy 之前，但它不是官方源——github 仍是 KindPython 唯一的官方源。
func TestDefaultCatalog_PythonHasAstralBeforeGhProxy(t *testing.T) {
	sources := mustDefaultCatalog(t).Sources(KindPython)
	position := make(map[string]int, len(sources))
	officialKeys := make([]string, 0, 1)
	for index, source := range sources {
		position[source.Key()] = index
		if source.Official() {
			officialKeys = append(officialKeys, source.Key())
		}
	}
	astral, ok := position["astral"]
	if !ok {
		t.Fatalf("python sources = %v, want astral", sourceKeys(sources))
	}
	ghProxy, ok := position["gh-proxy"]
	if !ok {
		t.Fatalf("python sources = %v, want gh-proxy", sourceKeys(sources))
	}
	if astral >= ghProxy {
		t.Fatalf("astral index %d is not before gh-proxy index %d", astral, ghProxy)
	}
	if got, want := sources[astral].BaseURL(),
		"https://releases.astral.sh/github/python-build-standalone/releases/download"; got != want {
		t.Fatalf("astral base URL = %q, want %q", got, want)
	}
	if sources[astral].Official() {
		t.Fatal("astral is official, want non-official")
	}
	if len(officialKeys) != 1 || officialKeys[0] != "github" {
		t.Fatalf("official python sources = %v, want only github", officialKeys)
	}
}

func sourceKeys(sources []Source) []string {
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		keys = append(keys, source.Key())
	}
	return keys
}
