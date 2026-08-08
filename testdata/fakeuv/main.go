package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type replayConfig struct {
	ExitCode int           `json:"exitCode"`
	Stdout   []string      `json:"stdout"`
	Stderr   []string      `json:"stderr"`
	DelayMS  int           `json:"delayMs"`
	Events   []replayEvent `json:"events"`
}

type replayEvent struct {
	Stream  string `json:"stream"`
	Line    string `json:"line"`
	DelayMS int    `json:"delayMs"`
}

type invocationRecord struct {
	Arguments   []string          `json:"arguments"`
	Environment map[string]string `json:"environment"`
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
	if path := os.Getenv("FAKE_UV_RECORD"); path != "" {
		environment := make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, ok := splitEnvironment(entry)
			if ok {
				environment[key] = value
			}
		}
		record, err := json.Marshal(invocationRecord{
			Arguments:   append([]string(nil), os.Args[1:]...),
			Environment: environment,
		})
		if err != nil {
			os.Exit(92)
		}
		if err := os.WriteFile(path, record, 0o600); err != nil {
			os.Exit(92)
		}
	}
	if config.DelayMS > 0 {
		time.Sleep(time.Duration(config.DelayMS) * time.Millisecond)
	}
	if len(config.Events) > 0 {
		for _, event := range config.Events {
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
		for _, line := range config.Stdout {
			fmt.Fprintln(os.Stdout, line)
		}
		for _, line := range config.Stderr {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	os.Exit(config.ExitCode)
}

func splitEnvironment(entry string) (string, string, bool) {
	for index, character := range entry {
		if character == '=' {
			return entry[:index], entry[index+1:], true
		}
	}
	return "", "", false
}
