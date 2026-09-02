// Package main 提供 M6 进程监督测试使用的可配置假后端。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fakeBackendConfigEnv  = "FAKE_BACKEND_CONFIG"
	fakeBackendRoleEnv    = "FAKE_BACKEND_ROLE"
	grandchildLifetimeEnv = "FAKE_BACKEND_GRANDCHILD_LIFETIME_MS"
	grandchildDetachedEnv = "FAKE_BACKEND_GRANDCHILD_DETACHED"
	grandchildRole        = "grandchild"
	healthPath            = "/api/core/health"
	closePath             = "/api/core/close"
	// supervisedPortEnv 是增补 1 C12 下 Runtime 注入的端口；listenAddress 留空时假后端
	// 像真后端一样只认它，缺失或非法则回退缺省地址。
	supervisedPortEnv    = "AUTO_MAS_SUPERVISED_PORT"
	defaultListenAddress = "127.0.0.1:36163"
	minSupervisedPort    = 1024
	maxSupervisedPort    = 65535
)

type fakeBackendConfig struct {
	ListenAddress string `json:"listenAddress"`
	ListenDelayMS int    `json:"listenDelayMs"`
	ReadyFile     string `json:"readyFile"`
	PIDFile       string `json:"pidFile"`
	// WorkingDirFile 让假后端报告自己的 os.Getwd()，供 T13.1 端到端断言
	// Runtime 设定的工作目录真的生效；父进程侧的 StartSpec 断言证明不了这件事。
	WorkingDirFile string `json:"workingDirFile"`
	// EnvironmentFile 让假后端把自己进程里读到的受监督环境变量落盘，供 T13.5
	// 端到端断言增补 1 C11 的四个变量确实穿过 uv 到达了真实后端进程；父进程侧
	// 的 StartSpec 断言只能证明 Runtime 传了什么，证明不了后端收到了什么。
	EnvironmentFile string `json:"environmentFile"`
	// ShutdownFile 只在「收到 close 并完成 server.Shutdown」的优雅路径上落盘，被 Job 硬杀时
	// 永远不会出现；T13.8 的 E2E 据此区分「HTTP 优雅关闭」与「被杀」。
	ShutdownFile         string `json:"shutdownFile"`
	GrandchildPIDFile    string `json:"grandchildPidFile"`
	SpawnGrandchild      bool   `json:"spawnGrandchild"`
	GrandchildLifetimeMS int    `json:"grandchildLifetimeMs"`
	// LeaveGrandchildOnCrash / LeaveGrandchildOnShutdown 都让孙进程脱离父进程的
	// liveness 管道并跳过自清理，区别只是在崩溃还是优雅关闭路径上留下它。
	// 后者用于证明「真有存活后代」时 Runtime 仍会强制回收并发出警告。
	LeaveGrandchildOnCrash    bool             `json:"leaveGrandchildOnCrash"`
	LeaveGrandchildOnShutdown bool             `json:"leaveGrandchildOnShutdown"`
	Health                    []healthResponse `json:"health"`
	HealthRaw                 []string         `json:"healthRaw"`
	HealthHTTPStatus          []int            `json:"healthHttpStatus"`
	CloseStatus               int              `json:"closeStatus"`
	CrashAfterHealthRequests  int              `json:"crashAfterHealthRequests"`
	CrashExitCode             int              `json:"crashExitCode"`
	Events                    []outputEvent    `json:"events"`
	Stdout                    []outputEvent    `json:"stdout"`
	Stderr                    []outputEvent    `json:"stderr"`
}

type healthResponse struct {
	Ready            bool    `json:"ready"`
	BackgroundStatus string  `json:"backgroundStatus"`
	BackgroundError  *string `json:"backgroundError"`
	Protocol         int     `json:"protocol"`
	Version          string  `json:"version"`
	Commit           string  `json:"commit"`
}

type outputEvent struct {
	Stream    string `json:"stream"`
	Line      string `json:"line"`
	DelayMS   int    `json:"delayMs"`
	NoNewline bool   `json:"noNewline"`
}

type fakeBackend struct {
	config   fakeBackendConfig
	exit     func(int)
	shutdown func()

	// mu 保护 healthRequests；两个 Once 分别串行化崩溃和关闭回调。
	mu             sync.Mutex
	healthRequests int
	crashOnce      sync.Once
	shutdownOnce   sync.Once
}

func newFakeBackend(config fakeBackendConfig, exit func(int), shutdown func()) *fakeBackend {
	if exit == nil {
		exit = func(int) {}
	}
	if shutdown == nil {
		shutdown = func() {}
	}
	return &fakeBackend{config: config, exit: exit, shutdown: shutdown}
}

func (b *fakeBackend) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, b.handleHealth)
	mux.HandleFunc(closePath, b.handleClose)
	return mux
}

func (b *fakeBackend) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	b.mu.Lock()
	b.healthRequests++
	requestCount := b.healthRequests
	reply := healthResponse{
		Ready:            true,
		BackgroundStatus: "ready",
		Protocol:         1,
	}
	if len(b.config.Health) > 0 {
		index := requestCount - 1
		if index >= len(b.config.Health) {
			index = len(b.config.Health) - 1
		}
		reply = b.config.Health[index]
	}
	crash := b.config.CrashAfterHealthRequests > 0 &&
		requestCount >= b.config.CrashAfterHealthRequests
	exitCode := b.config.CrashExitCode
	if exitCode == 0 {
		exitCode = 71
	}
	b.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	status := sequenceValue(b.config.HealthHTTPStatus, requestCount, http.StatusOK)
	writer.WriteHeader(status)
	if len(b.config.HealthRaw) > 0 {
		_, _ = writer.Write([]byte(sequenceValue(b.config.HealthRaw, requestCount, "") + "\n"))
	} else {
		if err := json.NewEncoder(writer).Encode(reply); err != nil {
			return
		}
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if crash {
		b.crashOnce.Do(func() { go b.exit(exitCode) })
	}
}

func sequenceValue[T any](values []T, requestCount int, fallback T) T {
	if len(values) == 0 {
		return fallback
	}
	index := requestCount - 1
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func (b *fakeBackend) handleClose(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status := b.config.CloseStatus
	if status == 0 {
		status = http.StatusOK
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte("{\"accepted\":" + strconv.FormatBool(status >= 200 && status < 300) + "}\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if status >= 200 && status < 300 {
		b.shutdownOnce.Do(func() { go b.shutdown() })
	}
}

func main() {
	if os.Getenv(fakeBackendRoleEnv) == grandchildRole {
		runGrandchild()
		return
	}
	os.Exit(runFakeBackend())
}

func runFakeBackend() int {
	config, err := loadFakeBackendConfig(os.Getenv(fakeBackendConfigEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 90
	}
	if err := emitConfiguredOutput(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 91
	}
	if config.PIDFile != "" {
		if err := writeSignalFile(config.PIDFile, []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 98
		}
	}
	if config.WorkingDirFile != "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 97
		}
		if err := writeSignalFile(config.WorkingDirFile, []byte(filepath.Clean(workingDirectory)+"\n")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 97
		}
	}
	if config.EnvironmentFile != "" {
		if err := writeSignalFile(config.EnvironmentFile, supervisedEnvironmentReport()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 89
		}
	}

	grandchild, err := startGrandchild(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 92
	}
	cleanupGrandchild := true
	if grandchild != nil {
		defer func() {
			if cleanupGrandchild {
				stopGrandchild(grandchild)
			}
		}()
	}

	if config.ListenDelayMS > 0 {
		timer := time.NewTimer(time.Duration(config.ListenDelayMS) * time.Millisecond)
		<-timer.C
	}
	address := config.ListenAddress
	if address == "" {
		address = listenAddressFromEnvironment(os.Getenv(supervisedPortEnv))
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 93
	}

	exitRequests := make(chan int, 1)
	shutdownRequests := make(chan struct{}, 1)
	backend := newFakeBackend(config, func(code int) {
		select {
		case exitRequests <- code:
		default:
		}
	}, func() {
		select {
		case shutdownRequests <- struct{}{}:
		default:
		}
	})
	server := &http.Server{
		Handler:           backend.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()

	if config.ReadyFile != "" {
		baseURL := "http://" + listener.Addr().String()
		if err := writeSignalFile(config.ReadyFile, []byte(baseURL+"\n")); err != nil {
			_ = server.Close()
			fmt.Fprintln(os.Stderr, err)
			return 94
		}
	}

	select {
	case code := <-exitRequests:
		_ = server.Close()
		if config.LeaveGrandchildOnCrash {
			cleanupGrandchild = false
		}
		if code == 0 {
			return 71
		}
		return code
	case <-shutdownRequests:
		if config.LeaveGrandchildOnShutdown {
			cleanupGrandchild = false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := server.Shutdown(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 95
		}
		if config.ShutdownFile != "" {
			if err := writeSignalFile(config.ShutdownFile, []byte("graceful\n")); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 88
			}
		}
		return 0
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
			return 96
		}
		return 0
	}
}

// listenAddressFromEnvironment 按 C12 解释 AUTO_MAS_SUPERVISED_PORT：十进制且落在
// 合法范围内才采用，否则按缺失处理回退缺省地址——与真后端的回退语义一致。
func listenAddressFromEnvironment(raw string) string {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < minSupervisedPort || port > maxSupervisedPort {
		return defaultListenAddress
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func loadFakeBackendConfig(path string) (fakeBackendConfig, error) {
	if path == "" {
		return fakeBackendConfig{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return fakeBackendConfig{}, fmt.Errorf("read fake backend config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var config fakeBackendConfig
	if err := decoder.Decode(&config); err != nil {
		return fakeBackendConfig{}, fmt.Errorf("decode fake backend config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fakeBackendConfig{}, fmt.Errorf("decode fake backend config: %w", err)
	}
	if err := validateFakeBackendConfig(config); err != nil {
		return fakeBackendConfig{}, err
	}
	return config, nil
}

func validateFakeBackendConfig(config fakeBackendConfig) error {
	if config.ListenAddress != "" {
		host, _, err := net.SplitHostPort(config.ListenAddress)
		if err != nil {
			return fmt.Errorf("validate fake backend listenAddress: %w", err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("validate fake backend listenAddress: host must be a loopback IP")
		}
	}
	if err := validateMilliseconds("listenDelayMs", config.ListenDelayMS, 60_000); err != nil {
		return err
	}
	if err := validateMilliseconds("grandchildLifetimeMs", config.GrandchildLifetimeMS, 86_400_000); err != nil {
		return err
	}
	if config.CrashAfterHealthRequests < 0 {
		return errors.New("validate fake backend crashAfterHealthRequests: must be non-negative")
	}
	if config.CrashExitCode < 0 || config.CrashExitCode > 255 {
		return errors.New("validate fake backend crashExitCode: must be between 0 and 255")
	}
	if config.CloseStatus != 0 && (config.CloseStatus < 200 || config.CloseStatus > 599) {
		return errors.New("validate fake backend closeStatus: must be zero or a final HTTP status")
	}
	if len(config.Health) > 0 && len(config.HealthRaw) > 0 {
		return errors.New("validate fake backend health: health and healthRaw are mutually exclusive")
	}
	for _, status := range config.HealthHTTPStatus {
		if status < 200 || status > 599 {
			return errors.New("validate fake backend healthHttpStatus: values must be final HTTP statuses")
		}
	}
	for _, event := range config.Events {
		if event.Stream != "stdout" && event.Stream != "stderr" {
			return errors.New("validate fake backend events: stream must be stdout or stderr")
		}
		if err := validateMilliseconds("events.delayMs", event.DelayMS, 60_000); err != nil {
			return err
		}
	}
	for _, events := range [][]outputEvent{config.Stdout, config.Stderr} {
		for _, event := range events {
			if err := validateMilliseconds("output.delayMs", event.DelayMS, 60_000); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMilliseconds(field string, value int, maximum int) error {
	if value < 0 || value > maximum {
		return fmt.Errorf("validate fake backend %s: must be between 0 and %d", field, maximum)
	}
	return nil
}

func emitConfiguredOutput(config fakeBackendConfig) error {
	events := config.Events
	if len(events) == 0 {
		events = make([]outputEvent, 0, len(config.Stdout)+len(config.Stderr))
		for _, event := range config.Stdout {
			event.Stream = "stdout"
			events = append(events, event)
		}
		for _, event := range config.Stderr {
			event.Stream = "stderr"
			events = append(events, event)
		}
	}
	for _, event := range events {
		if event.DelayMS > 0 {
			timer := time.NewTimer(time.Duration(event.DelayMS) * time.Millisecond)
			<-timer.C
		}
		writer := os.Stdout
		if event.Stream == "stderr" {
			writer = os.Stderr
		}
		var err error
		if event.NoNewline {
			_, err = fmt.Fprint(writer, event.Line)
		} else {
			_, err = fmt.Fprintln(writer, event.Line)
		}
		if err != nil {
			return fmt.Errorf("write fake backend %s: %w", event.Stream, err)
		}
	}
	return nil
}

type grandchildProcess struct {
	command     *exec.Cmd
	cancel      context.CancelFunc
	parentAlive *os.File
}

func startGrandchild(config fakeBackendConfig) (*grandchildProcess, error) {
	if !config.SpawnGrandchild {
		return nil, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve fake backend executable: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var parentRead *os.File
	var parentWrite *os.File
	detached := config.LeaveGrandchildOnCrash || config.LeaveGrandchildOnShutdown
	if detached {
		parentRead, err = os.Open(os.DevNull)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("open fake backend grandchild stdin: %w", err)
		}
	} else {
		parentRead, parentWrite, err = os.Pipe()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("create fake backend parent-liveness pipe: %w", err)
		}
	}
	command := exec.CommandContext(ctx, executable)
	command.Env = append(
		os.Environ(),
		fakeBackendRoleEnv+"="+grandchildRole,
		grandchildLifetimeEnv+"="+strconv.Itoa(config.GrandchildLifetimeMS),
	)
	if detached {
		command.Env = append(command.Env, grandchildDetachedEnv+"=1")
	}
	command.Stdin = parentRead
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = parentRead.Close()
		if parentWrite != nil {
			_ = parentWrite.Close()
		}
		cancel()
		return nil, fmt.Errorf("start fake backend grandchild: %w", err)
	}
	_ = parentRead.Close()
	process := &grandchildProcess{command: command, cancel: cancel, parentAlive: parentWrite}
	if config.GrandchildPIDFile != "" {
		payload := []byte(strconv.Itoa(command.Process.Pid) + "\n")
		if err := writeSignalFile(config.GrandchildPIDFile, payload); err != nil {
			stopGrandchild(process)
			return nil, fmt.Errorf("write fake backend grandchild pid: %w", err)
		}
	}
	return process, nil
}

// supervisedEnvironmentReport 逐行输出 `<键>=<值>`，缺席的键整行不出现，
// 因此断言方能区分「注入了空串」和「根本没注入」。
func supervisedEnvironmentReport() []byte {
	var builder bytes.Buffer
	for _, key := range []string{
		"AUTO_MAS_UV_CACHE_DIR",
		"AUTO_MAS_UV_PYTHON_INSTALL_DIR",
		"AUTO_MAS_MIRROR_PACKAGE_INDEX",
		"AUTO_MAS_MIRROR_PYTHON",
	} {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(value)
		builder.WriteString("\n")
	}
	return builder.Bytes()
}

func writeSignalFile(path string, payload []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".fakebackend-signal-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func stopGrandchild(process *grandchildProcess) {
	if process == nil || process.command == nil {
		return
	}
	if process.parentAlive != nil {
		_ = process.parentAlive.Close()
	}
	process.cancel()
	_ = process.command.Wait()
}

func runGrandchild() {
	duration := 24 * time.Hour
	if raw := os.Getenv(grandchildLifetimeEnv); raw != "" {
		if milliseconds, err := strconv.Atoi(raw); err == nil && milliseconds > 0 {
			duration = time.Duration(milliseconds) * time.Millisecond
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	if os.Getenv(grandchildDetachedEnv) == "1" {
		<-timer.C
		return
	}
	parentGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(parentGone)
	}()
	select {
	case <-timer.C:
	case <-parentGone:
	}
}
