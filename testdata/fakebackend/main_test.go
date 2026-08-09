package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fakeBackendHTTPClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	Timeout:   2 * time.Second,
}

func TestFakeBackend_HealthAndCloseScenarios(t *testing.T) {
	backgroundFailure := "plugin initialization failed"
	config := fakeBackendConfig{
		Health: []healthResponse{
			{
				Ready:            false,
				BackgroundStatus: "starting",
				Protocol:         1,
				Version:          "v6.0.0-test",
				Commit:           strings.Repeat("a", 40),
			},
			{
				Ready:            false,
				BackgroundStatus: "running",
				Protocol:         1,
				Version:          "v6.0.0-test",
				Commit:           strings.Repeat("a", 40),
			},
			{
				Ready:            true,
				BackgroundStatus: "ready",
				Protocol:         1,
				Version:          "v6.0.0-test",
				Commit:           strings.Repeat("a", 40),
			},
			{
				Ready:            false,
				BackgroundStatus: "failed",
				BackgroundError:  &backgroundFailure,
				Protocol:         9,
				Version:          "wrong-version",
				Commit:           strings.Repeat("b", 40),
			},
		},
		CrashAfterHealthRequests: 4,
		CrashExitCode:            73,
	}
	exitCodes := make(chan int, 1)
	shutdowns := make(chan struct{}, 1)
	backend := newFakeBackend(config, func(code int) {
		exitCodes <- code
	}, func() {
		shutdowns <- struct{}{}
	})
	server := httptest.NewServer(backend.Handler())
	t.Cleanup(server.Close)

	for index, wantStatus := range []string{"starting", "running", "ready", "failed"} {
		response, err := fakeBackendHTTPClient.Get(server.URL + healthPath)
		if err != nil {
			t.Fatalf("health request %d: %v", index+1, err)
		}
		var health healthResponse
		decodeJSONResponse(t, response, &health)
		if health.BackgroundStatus != wantStatus {
			t.Fatalf("health request %d status = %q, want %q", index+1, health.BackgroundStatus, wantStatus)
		}
		if index == 2 && (health.Protocol != 1 || health.Version != "v6.0.0-test" || health.Commit != strings.Repeat("a", 40)) {
			t.Fatalf("ready health identity = %#v", health)
		}
		if index == 3 {
			if health.BackgroundError == nil || *health.BackgroundError != backgroundFailure {
				t.Fatalf("failed health backgroundError = %#v", health.BackgroundError)
			}
			if health.Protocol != 9 || health.Version != "wrong-version" || health.Commit != strings.Repeat("b", 40) {
				t.Fatalf("failed health identity = %#v", health)
			}
		}
	}

	select {
	case code := <-exitCodes:
		if code != 73 {
			t.Fatalf("crash exit code = %d, want 73", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake backend did not request configured crash")
	}
	response, err := fakeBackendHTTPClient.Get(server.URL + healthPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	select {
	case code := <-exitCodes:
		t.Fatalf("duplicate health triggered second crash with code %d", code)
	case <-time.After(25 * time.Millisecond):
	}

	t.Run("close accepted", func(t *testing.T) {
		accepted := newFakeBackend(fakeBackendConfig{}, func(int) {}, func() {
			shutdowns <- struct{}{}
		})
		server := httptest.NewServer(accepted.Handler())
		defer server.Close()
		response, err := fakeBackendHTTPClient.Post(server.URL+closePath, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("close status = %d, want 200", response.StatusCode)
		}
		select {
		case <-shutdowns:
		case <-time.After(2 * time.Second):
			t.Fatal("accepted close did not request shutdown")
		}
		response, err = fakeBackendHTTPClient.Post(server.URL+closePath, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		select {
		case <-shutdowns:
			t.Fatal("duplicate close triggered second shutdown")
		case <-time.After(25 * time.Millisecond):
		}
	})

	t.Run("close refused", func(t *testing.T) {
		refused := newFakeBackend(fakeBackendConfig{CloseStatus: http.StatusServiceUnavailable}, func(int) {}, func() {
			t.Fatal("refused close requested shutdown")
		})
		server := httptest.NewServer(refused.Handler())
		defer server.Close()
		response, err := fakeBackendHTTPClient.Post(server.URL+closePath, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("close status = %d, want 503", response.StatusCode)
		}
	})

	t.Run("raw health and strict config", func(t *testing.T) {
		raw := `{"ready":"wrong-type","protocol":"wrong"}`
		backend := newFakeBackend(fakeBackendConfig{
			HealthRaw:        []string{raw},
			HealthHTTPStatus: []int{http.StatusTeapot},
		}, nil, nil)
		server := httptest.NewServer(backend.Handler())
		defer server.Close()
		response, err := fakeBackendHTTPClient.Get(server.URL + healthPath)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusTeapot || strings.TrimSpace(string(body)) != raw {
			t.Fatalf("raw response = status %d body %q", response.StatusCode, body)
		}

		root := t.TempDir()
		for name, payload := range map[string]string{
			"unknown field":     `{"unknown":true}`,
			"invalid stream":    `{"events":[{"stream":"other"}]}`,
			"invalid status":    `{"closeStatus":42}`,
			"multiple payloads": `{}` + "\n" + `{}`,
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(root, strings.ReplaceAll(name, " ", "-")+".json")
				if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadFakeBackendConfig(path); err == nil {
					t.Fatal("invalid config was accepted")
				}
			})
		}
	})

	t.Run("concurrent triggers fire once", func(t *testing.T) {
		var crashCount atomic.Int32
		var shutdownCount atomic.Int32
		backend := newFakeBackend(fakeBackendConfig{CrashAfterHealthRequests: 1}, func(int) {
			crashCount.Add(1)
		}, func() {
			shutdownCount.Add(1)
		})
		server := httptest.NewServer(backend.Handler())
		defer server.Close()
		errorsFound := make(chan error, 32)
		var group sync.WaitGroup
		for index := 0; index < 16; index++ {
			group.Add(2)
			go func() {
				defer group.Done()
				response, err := fakeBackendHTTPClient.Get(server.URL + healthPath)
				if err == nil {
					err = response.Body.Close()
				}
				if err != nil {
					errorsFound <- err
				}
			}()
			go func() {
				defer group.Done()
				response, err := fakeBackendHTTPClient.Post(server.URL+closePath, "application/json", nil)
				if err == nil {
					err = response.Body.Close()
				}
				if err != nil {
					errorsFound <- err
				}
			}()
		}
		group.Wait()
		close(errorsFound)
		for err := range errorsFound {
			t.Error(err)
		}
		waitForAtomicValue(t, &crashCount, 1)
		waitForAtomicValue(t, &shutdownCount, 1)
		if crashCount.Load() != 1 || shutdownCount.Load() != 1 {
			t.Fatalf("callbacks = crash %d shutdown %d, want one each", crashCount.Load(), shutdownCount.Load())
		}
	})
}

func waitForAtomicValue(t *testing.T, value *atomic.Int32, want int32) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for value.Load() != want {
		select {
		case <-timer.C:
			t.Fatalf("atomic value = %d, want %d", value.Load(), want)
		case <-ticker.C:
		}
	}
}

func TestFakeBackend_StreamsAndGrandchild(t *testing.T) {
	root := t.TempDir()
	executable := buildFakeBackend(t, root)
	readyFile := filepath.Join(root, "ready.txt")
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	configPath := filepath.Join(root, "config.json")
	config := fakeBackendConfig{
		ListenAddress:        "127.0.0.1:0",
		ListenDelayMS:        40,
		ReadyFile:            readyFile,
		GrandchildPIDFile:    grandchildPIDFile,
		SpawnGrandchild:      true,
		GrandchildLifetimeMS: 5000,
		Stdout: []outputEvent{
			{Line: "stdout-first"},
			{Line: "stdout-tail", NoNewline: true},
		},
		Stderr: []outputEvent{{Line: "stderr-line"}},
		Health: []healthResponse{{
			Ready:            true,
			BackgroundStatus: "ready",
			Protocol:         1,
			Version:          "v6.0.0-test",
			Commit:           strings.Repeat("c", 40),
		}},
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	commandContext, cancelCommand := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancelCommand()
	command := exec.CommandContext(commandContext, executable)
	command.Env = append(os.Environ(), fakeBackendConfigEnv+"="+configPath)
	command.Stdout = &stdout
	command.Stderr = &stderr
	startedAt := time.Now()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	commandWaited := false
	t.Cleanup(func() {
		cancelCommand()
		if command.Process != nil && !commandWaited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		ensureRecordedProcessStopped(t, grandchildPIDFile)
	})

	baseURL := strings.TrimSpace(waitForFile(t, readyFile))
	if elapsed := time.Since(startedAt); elapsed < 30*time.Millisecond {
		t.Fatalf("ready after %s, configured listen delay was 40ms", elapsed)
	}
	grandchildPIDText := strings.TrimSpace(waitForFile(t, grandchildPIDFile))
	grandchildPID, err := strconv.Atoi(grandchildPIDText)
	if err != nil || grandchildPID <= 0 {
		t.Fatalf("grandchild PID = %q, err = %v", grandchildPIDText, err)
	}
	response, err := fakeBackendHTTPClient.Get(baseURL + healthPath)
	if err != nil {
		t.Fatal(err)
	}
	var health healthResponse
	decodeJSONResponse(t, response, &health)
	if !health.Ready || health.BackgroundStatus != "ready" || health.Protocol != 1 || health.Version != "v6.0.0-test" || health.Commit != strings.Repeat("c", 40) {
		t.Fatalf("health = %#v", health)
	}
	response, err = fakeBackendHTTPClient.Post(baseURL+closePath, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("close status = %d, want 200", response.StatusCode)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("fake backend exit: %v", err)
	}
	commandWaited = true
	waitRecordedProcessStopped(t, grandchildPIDFile)
	if got := stdout.String(); got != "stdout-first\nstdout-tail" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "stderr-line\n" {
		t.Fatalf("stderr = %q", got)
	}

	t.Run("compiled process crash", func(t *testing.T) {
		crashReadyFile := filepath.Join(root, "crash-ready.txt")
		crashGrandchildPIDFile := filepath.Join(root, "crash-grandchild.pid")
		crashConfigPath := filepath.Join(root, "crash-config.json")
		crashConfig := fakeBackendConfig{
			ListenAddress:            "127.0.0.1:0",
			ListenDelayMS:            20,
			ReadyFile:                crashReadyFile,
			GrandchildPIDFile:        crashGrandchildPIDFile,
			SpawnGrandchild:          true,
			GrandchildLifetimeMS:     10_000,
			LeaveGrandchildOnCrash:   true,
			CrashAfterHealthRequests: 1,
			CrashExitCode:            73,
			HealthRaw:                []string{`{"ready":true,"backgroundStatus":"ready","protocol":1}`},
		}
		writeConfig(t, crashConfigPath, crashConfig)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		crashCommand := exec.CommandContext(ctx, executable)
		crashCommand.Env = append(os.Environ(), fakeBackendConfigEnv+"="+crashConfigPath)
		pipeRead, pipeWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pipeRead.Close() }()
		crashCommand.Stdout = pipeWrite
		crashCommand.Stderr = pipeWrite
		if err := crashCommand.Start(); err != nil {
			_ = pipeWrite.Close()
			t.Fatal(err)
		}
		if err := pipeWrite.Close(); err != nil {
			t.Fatal(err)
		}
		crashWaited := false
		t.Cleanup(func() {
			cancel()
			if crashCommand.Process != nil && !crashWaited {
				_ = crashCommand.Process.Kill()
				_ = crashCommand.Wait()
			}
			ensureRecordedProcessStopped(t, crashGrandchildPIDFile)
		})
		crashURL := strings.TrimSpace(waitForFile(t, crashReadyFile))
		_ = waitForFile(t, crashGrandchildPIDFile)
		response, err := fakeBackendHTTPClient.Get(crashURL + healthPath)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		err = crashCommand.Wait()
		crashWaited = true
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 73 {
			t.Fatalf("crash exit = %v, want exit code 73", err)
		}
		grandchildPID, err := readRecordedPID(crashGrandchildPIDFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := waitTestProcessExit(grandchildPID, 50*time.Millisecond); err == nil {
			t.Fatal("detached grandchild exited with its parent")
		}
		pipeEOF := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, pipeRead)
			pipeEOF <- err
		}()
		select {
		case err := <-pipeEOF:
			t.Fatalf("descendant-held pipe reached EOF before cleanup: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if err := terminateTestProcess(grandchildPID); err != nil {
			t.Fatal(err)
		}
		if err := waitTestProcessExit(grandchildPID, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-pipeEOF:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("descendant-held pipe did not close after cleanup")
		}
	})
}

func writeConfig(t *testing.T, path string, config fakeBackendConfig) {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeJSONResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("response status = %d, body = %q", response.StatusCode, body)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func buildFakeBackend(t *testing.T, root string) string {
	t.Helper()
	name := "fakebackend"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(root, name)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", executable, ".")
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake backend: %v\n%s", err, output)
	}
	return executable
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		payload, err := os.ReadFile(path)
		if err == nil && len(payload) > 0 && payload[len(payload)-1] == '\n' {
			return string(payload)
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s: %v", path, err)
		case <-ticker.C:
		}
	}
}

func readRecordedPID(path string) (int, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		return 0, fmt.Errorf("invalid recorded pid %q: %w", payload, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid recorded pid %q: must be positive", payload)
	}
	return pid, nil
}

func waitRecordedProcessStopped(t *testing.T, path string) {
	t.Helper()
	pid, err := readRecordedPID(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTestProcessExit(pid, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func ensureRecordedProcessStopped(t *testing.T, path string) {
	t.Helper()
	pid, err := readRecordedPID(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Errorf("read grandchild PID: %v", err)
		return
	}
	if err := waitTestProcessExit(pid, 250*time.Millisecond); err == nil {
		return
	}
	if err := terminateTestProcess(pid); err != nil {
		t.Errorf("terminate grandchild PID %d: %v", pid, err)
		return
	}
	if err := waitTestProcessExit(pid, 2*time.Second); err != nil {
		t.Errorf("wait grandchild PID %d: %v", pid, err)
	}
}
