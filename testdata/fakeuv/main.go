package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type replayConfig struct {
	replayAction
	Rules []replayRule `json:"rules"`
}

type replayRule struct {
	replayAction
	ArgumentsPrefix []string `json:"argumentsPrefix"`
}

type replayAction struct {
	ExitCode          int           `json:"exitCode"`
	Stdout            []string      `json:"stdout"`
	Stderr            []string      `json:"stderr"`
	DelayMS           int           `json:"delayMs"`
	Events            []replayEvent `json:"events"`
	CreateDirectories []string      `json:"createDirectories"`
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
	action := config.replayAction
	for _, rule := range config.Rules {
		if hasArgumentsPrefix(os.Args[1:], rule.ArgumentsPrefix) {
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
			Arguments:   append([]string(nil), os.Args[1:]...),
			Environment: environment,
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
