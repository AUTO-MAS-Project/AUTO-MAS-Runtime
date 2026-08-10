//go:build integration

package version

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ldflagsTestVersion = "v9.9.9-beta.2"
	ldflagsTestCommit  = "0123456789abcdef0123456789abcdef01234567"
	ldflagsTestDate    = "2026-08-10T12:34:56Z"
)

var ldflagsBinaryCache struct {
	sync.Once
	path string
	root string
	err  error
}

// TestMain 在全部测试结束后删除共享的链接器验证产物。
func TestMain(m *testing.M) {
	code := m.Run()
	if ldflagsBinaryCache.root != "" {
		if err := os.RemoveAll(ldflagsBinaryCache.root); err != nil {
			fmt.Fprintf(os.Stderr, "remove linker test directory %q: %v\n", ldflagsBinaryCache.root, err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

// TestVersion_LdflagsBinary 证明发布构建的链接器值会从同一个可执行文件到达
// 协议 hello 事件和 version 结果。
func TestVersion_LdflagsBinary(t *testing.T) {
	t.Parallel()
	executable := buildLdflagsBinary(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--output", "ndjson", "version")
	command.Stdin = strings.NewReader("")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run injected version binary: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	events := decodeLdflagsEvents(t, stdout.Bytes())
	if len(events) < 2 {
		t.Fatalf("event count = %d, want hello and result", len(events))
	}
	hello := events[0]
	if got := stringValue(t, hello, "type"); got != "hello" {
		t.Fatalf("first event type = %q, want hello", got)
	}
	if got := stringValue(t, hello, "runtimeVersion"); got != ldflagsTestVersion {
		t.Errorf("hello.runtimeVersion = %q, want %q", got, ldflagsTestVersion)
	}
	result := events[len(events)-1]
	if got := stringValue(t, result, "type"); got != "result" {
		t.Fatalf("last event type = %q, want result", got)
	}
	helloCount := 0
	resultCount := 0
	for index, event := range events {
		switch stringValue(t, event, "type") {
		case "hello":
			helloCount++
		case "result":
			resultCount++
		}
		sequence, ok := event["sequence"].(json.Number)
		if !ok {
			t.Fatalf("event %d sequence = %#v, want JSON number", index, event["sequence"])
		}
		want := json.Number(fmt.Sprintf("%d", index+1))
		if sequence != want {
			t.Errorf("event %d sequence = %s, want %s", index, sequence, want)
		}
	}
	if helloCount != 1 {
		t.Errorf("hello event count = %d, want 1", helloCount)
	}
	if resultCount != 1 {
		t.Errorf("result event count = %d, want 1", resultCount)
	}
	if success, ok := result["success"].(bool); !ok || !success {
		t.Errorf("result.success = %#v, want true", result["success"])
	}
	details, ok := result["details"].(map[string]any)
	if !ok {
		t.Fatalf("result.details = %#v, want object", result["details"])
	}
	for _, test := range []struct {
		field string
		want  string
	}{
		{field: "runtimeVersion", want: ldflagsTestVersion},
		{field: "commit", want: ldflagsTestCommit},
		{field: "buildDate", want: ldflagsTestDate},
	} {
		if got := stringValue(t, details, test.field); got != test.want {
			t.Errorf("result.details.%s = %q, want %q", test.field, got, test.want)
		}
	}
}

func buildLdflagsBinary(t *testing.T) string {
	t.Helper()
	ldflagsBinaryCache.Do(func() {
		_, source, _, ok := runtime.Caller(0)
		if !ok {
			ldflagsBinaryCache.err = errors.New("runtime.Caller() could not resolve test source")
			return
		}
		root, err := os.MkdirTemp("", "auto-mas-runtime-t71-")
		if err != nil {
			ldflagsBinaryCache.err = fmt.Errorf("create linker test directory: %w", err)
			return
		}
		ldflagsBinaryCache.root = root
		name := "auto-mas-runtime-ldflags"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		ldflagsBinaryCache.path = filepath.Join(root, name)
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
		ldflags := strings.Join([]string{
			"-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Version=" + ldflagsTestVersion,
			"-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Commit=" + ldflagsTestCommit,
			"-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.BuildDate=" + ldflagsTestDate,
		}, " ")
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(
			ctx,
			"go",
			"build",
			"-buildvcs=false",
			"-trimpath",
			"-ldflags",
			ldflags,
			"-o",
			ldflagsBinaryCache.path,
			"./cmd/auto-mas-runtime",
		)
		command.Dir = moduleRoot
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			ldflagsBinaryCache.err = fmt.Errorf("build injected version binary: %w: %s", buildErr, output)
		}
	})
	if ldflagsBinaryCache.err != nil {
		t.Fatal(ldflagsBinaryCache.err)
	}
	return ldflagsBinaryCache.path
}

func decodeLdflagsEvents(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var events []map[string]any
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			t.Fatal("NDJSON contains an empty line")
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var event map[string]any
		err := decoder.Decode(&event)
		if err != nil {
			t.Fatalf("decode NDJSON line: %v", err)
		}
		if event == nil {
			t.Fatal("NDJSON line is not a JSON object")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				t.Fatal("NDJSON line contains multiple JSON values")
			}
			t.Fatalf("decode trailing NDJSON value: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read NDJSON events: %v", err)
	}
	return events
}

func stringValue(t *testing.T, object map[string]any, field string) string {
	t.Helper()
	value, ok := object[field].(string)
	if !ok {
		t.Fatalf("field %q = %#v, want string", field, object[field])
	}
	return value
}
