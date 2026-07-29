package protocol_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readDoc 读取仓库 doc/ 下的权威文档。
//
// 冻结全集的测试刻意在运行时直接解析文档而不是复制一份表格，
// 这样文档与实现一旦分叉就立刻失败，而不是各自为政。
func readDoc(t *testing.T, name string) string {
	t.Helper()

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() could not resolve test source")
	}
	path := filepath.Join(filepath.Dir(source), "..", "..", "doc", name)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read doc/%s: %v", name, err)
	}
	return string(content)
}

// docSection 截取 start 与 end 两个标记之间的文档片段。
// 任一标记缺失都直接失败，避免解析静默退化为空片段后「零条也算通过」。
func docSection(t *testing.T, content, start, end string) string {
	t.Helper()

	startIndex := strings.Index(content, start)
	if startIndex < 0 {
		t.Fatalf("document missing start marker %q", start)
	}
	section := content[startIndex+len(start):]
	endIndex := strings.Index(section, end)
	if endIndex < 0 {
		t.Fatalf("document missing end marker %q", end)
	}
	return section[:endIndex]
}
