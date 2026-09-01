package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type replayConfig struct {
	replayAction
	Rules []replayRule `json:"rules"`
}

type replayRule struct {
	replayAction
	ArgumentsPrefix []string `json:"argumentsPrefix"`
	// ArgumentsContain 要求这些参数全部出现，用于匹配 --frozen 这类与位置无关的开关。
	ArgumentsContain []string `json:"argumentsContain"`
	// LockContains 要求 --project 指向目录里的 uv.lock 含该子串，
	// 用于按「改写成了哪个镜像」注入失败。
	LockContains string `json:"lockContains"`
}

// matches 报告规则的全部条件是否都满足；没有任何条件的规则不匹配。
func (r replayRule) matches(arguments []string, lock string) bool {
	if len(r.ArgumentsPrefix) == 0 && len(r.ArgumentsContain) == 0 && r.LockContains == "" {
		return false
	}
	if len(r.ArgumentsPrefix) > 0 && !hasArgumentsPrefix(arguments, r.ArgumentsPrefix) {
		return false
	}
	for _, needle := range r.ArgumentsContain {
		if !containsArgument(arguments, needle) {
			return false
		}
	}
	if r.LockContains != "" && !strings.Contains(lock, r.LockContains) {
		return false
	}
	return true
}

type replayAction struct {
	// ReadyFile 在动作开始时写出，ReleaseFile 出现前进程不返回；
	// 两者配合可以让测试在假 uv 正在运行时精确注入取消。
	ReadyFile         string        `json:"readyFile"`
	ReleaseFile       string        `json:"releaseFile"`
	ExitCode          int           `json:"exitCode"`
	Stdout            []string      `json:"stdout"`
	Stderr            []string      `json:"stderr"`
	DelayMS           int           `json:"delayMs"`
	Events            []replayEvent `json:"events"`
	CreateDirectories []string      `json:"createDirectories"`
	Exec              string        `json:"exec"`
	ExecArgs          []string      `json:"execArgs"`
	PIDFile           string        `json:"pidFile"`
	ExecReadyFile     string        `json:"execReadyFile"`
	ExecReleaseFile   string        `json:"execReleaseFile"`
}

type replayEvent struct {
	Stream  string `json:"stream"`
	Line    string `json:"line"`
	DelayMS int    `json:"delayMs"`
}

type invocationRecord struct {
	Arguments   []string          `json:"arguments"`
	Environment map[string]string `json:"environment"`
	// ProjectDir 是 --project 的取值，LockIndexPrefixes 是该目录 uv.lock 里出现的
	// 索引与 artifact 前缀（去重排序），测试据此断言实际用的是哪个镜像源。
	ProjectDir        string   `json:"projectDir,omitempty"`
	LockIndexPrefixes []string `json:"lockIndexPrefixes,omitempty"`
}

func main() {
	config := replayConfig{}
	if path := os.Getenv("FAKE_UV_CONFIG"); path != "" {
		payload, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(90)
		}
		if err := json.Unmarshal(payload, &config); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(91)
		}
	}
	arguments := os.Args[1:]
	projectDir := argumentValue(arguments, "--project")
	lock := readProjectLock(projectDir)
	action := config.replayAction
	for _, rule := range config.Rules {
		if rule.matches(arguments, lock) {
			action = rule.replayAction
			break
		}
	}
	if path := os.Getenv("FAKE_UV_RECORD"); path != "" {
		environment := make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, ok := splitEnvironment(entry)
			if ok {
				environment[key] = value
			}
		}
		record := invocationRecord{
			Arguments:         append([]string(nil), arguments...),
			Environment:       environment,
			ProjectDir:        projectDir,
			LockIndexPrefixes: lockIndexPrefixes(lock),
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(92)
		}
		encodeErr := json.NewEncoder(file).Encode(record)
		closeErr := file.Close()
		if encodeErr != nil || closeErr != nil {
			os.Exit(92)
		}
	}
	for _, path := range action.CreateDirectories {
		if path == "" || os.MkdirAll(path, 0o700) != nil {
			os.Exit(93)
		}
	}
	if action.PIDFile != "" {
		if err := writeSignalFile(action.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid()))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(94)
		}
	}
	if action.Exec != "" {
		command := exec.Command(action.Exec, action.ExecArgs...)
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		command.Env = os.Environ()
		runErr := command.Run()
		if action.ExecReadyFile != "" {
			if err := writeSignalFile(action.ExecReadyFile, []byte("ready\n")); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(95)
			}
		}
		if action.ExecReleaseFile != "" {
			if err := waitForSignalFile(action.ExecReleaseFile); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(95)
			}
		}
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintln(os.Stderr, runErr)
			os.Exit(94)
		}
		os.Exit(0)
	}
	if action.ReadyFile != "" {
		if err := writeSignalFile(action.ReadyFile, []byte("ready\n")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(96)
		}
	}
	if action.ReleaseFile != "" {
		if err := waitForSignalFile(action.ReleaseFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(96)
		}
	}
	if action.DelayMS > 0 {
		time.Sleep(time.Duration(action.DelayMS) * time.Millisecond)
	}
	if len(action.Events) > 0 {
		for _, event := range action.Events {
			if event.DelayMS > 0 {
				time.Sleep(time.Duration(event.DelayMS) * time.Millisecond)
			}
			switch event.Stream {
			case "stderr":
				fmt.Fprintln(os.Stderr, event.Line)
			default:
				fmt.Fprintln(os.Stdout, event.Line)
			}
		}
	} else {
		for _, line := range action.Stdout {
			fmt.Fprintln(os.Stdout, line)
		}
		for _, line := range action.Stderr {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	os.Exit(action.ExitCode)
}

// argumentValue 返回 name 后面紧跟的那个参数值。
func argumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func containsArgument(arguments []string, needle string) bool {
	for _, argument := range arguments {
		if argument == needle {
			return true
		}
	}
	return false
}

// readProjectLock 读取 --project 目录里的 uv.lock；缺失时返回空串。
func readProjectLock(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	payload, err := os.ReadFile(filepath.Join(projectDir, "uv.lock"))
	if err != nil {
		return ""
	}
	return string(payload)
}

// lockIndexPrefixes 提取锁文本里出现的索引与 artifact 前缀，去重后排序。
func lockIndexPrefixes(lock string) []string {
	unique := make(map[string]struct{})
	rest := lock
	for {
		start := strings.Index(rest, "https://")
		if start < 0 {
			break
		}
		rest = rest[start:]
		end := strings.IndexAny(rest, "\"' \t\r\n,")
		token := rest
		if end >= 0 {
			token = rest[:end]
			rest = rest[end:]
		}
		if prefix, ok := indexPrefixOf(token); ok {
			unique[prefix] = struct{}{}
		}
		if end < 0 {
			break
		}
	}
	prefixes := make([]string, 0, len(unique))
	for prefix := range unique {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

func indexPrefixOf(url string) (string, bool) {
	for _, marker := range []string{"/simple", "/packages/"} {
		if index := strings.Index(url, marker); index >= 0 {
			return url[:index+len(marker)], true
		}
	}
	return "", false
}

func hasArgumentsPrefix(arguments, prefix []string) bool {
	if len(prefix) == 0 || len(arguments) < len(prefix) {
		return false
	}
	for index := range prefix {
		if arguments[index] != prefix[index] {
			return false
		}
	}
	return true
}

func splitEnvironment(entry string) (string, string, bool) {
	for index, character := range entry {
		if character == '=' {
			return entry[:index], entry[index+1:], true
		}
	}
	return "", "", false
}

func writeSignalFile(path string, payload []byte) (resultErr error) {
	if path == "" {
		return errors.New("signal file path is empty")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".fakeuv-signal-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			resultErr = errors.Join(resultErr, os.Remove(temporaryPath))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(payload); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func waitForSignalFile(path string) error {
	if path == "" {
		return errors.New("release file path is empty")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		<-ticker.C
	}
}
