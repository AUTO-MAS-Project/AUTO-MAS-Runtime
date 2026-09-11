package mirror

import "testing"

// TestNewLoopbackRewrite_OnlyAcceptsLoopbackHTTP 锁定回环例外的形态：只有 http://127.0.0.1:<port> 可作改写目标。
func TestNewLoopbackRewrite_OnlyAcceptsLoopbackHTTP(t *testing.T) {
	t.Parallel()

	rewrite, err := NewLoopbackRewrite("http://127.0.0.1:39170")
	if err != nil {
		t.Fatalf("NewLoopbackRewrite() error = %v", err)
	}
	if rewrite.SimpleBase() != "http://127.0.0.1:39170/simple" || rewrite.PackagesBase() != "http://127.0.0.1:39170/packages/" {
		t.Fatalf("rewrite = %+v", rewrite)
	}
	if trailing, err := NewLoopbackRewrite("http://127.0.0.1:39170/"); err != nil || trailing != rewrite {
		t.Fatalf("trailing slash form = %+v, err = %v", trailing, err)
	}
	for _, bad := range []string{"https://127.0.0.1:1", "http://localhost:1", "http://127.0.0.1", "http://127.0.0.1:1/x", "http://user@127.0.0.1:1", "http://0.0.0.0:1", ""} {
		if _, err := NewLoopbackRewrite(bad); err == nil {
			t.Errorf("NewLoopbackRewrite(%q) error = nil, want error", bad)
		}
	}
	if !IsLoopbackBase("http://127.0.0.1:1/simple/") || IsLoopbackBase("https://pypi.org/simple/") {
		t.Error("IsLoopbackBase misclassified")
	}
}
