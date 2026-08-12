package telemetry

import (
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultFlushTimeout 是短命 Runtime 收口遥测时允许占用的最长时间。
	DefaultFlushTimeout = 500 * time.Millisecond

	envTelemetry             = "AUTO_MAS_TELEMETRY"
	envSentryDSN             = "AUTO_MAS_SENTRY_DSN"
	envSentryEnvironment     = "AUTO_MAS_SENTRY_ENVIRONMENT"
	envSentryRelease         = "AUTO_MAS_SENTRY_RELEASE"
	defaultSentryEnvironment = "production"
)

// BuildSentryDSN 是发布构建可通过 -ldflags 注入的 Sentry DSN。
// 源码和默认开发构建保持为空，避免凭据进入版本库。
var BuildSentryDSN = ""

// Config 是一次 Runtime 调用使用的遥测配置快照。
type Config struct {
	Enabled           bool
	Offline           bool
	SentryDSN         string
	SentryEnvironment string
	SentryRelease     string
	FlushTimeout      time.Duration
}

// LoadConfig 从当前进程环境读取遥测配置。
func LoadConfig() Config {
	return LoadConfigFrom(os.LookupEnv)
}

// LoadConfigFrom 使用调用方提供的环境读取函数构造配置，便于测试隔离进程环境。
func LoadConfigFrom(lookup func(string) (string, bool)) Config {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	config := Config{
		SentryEnvironment: defaultSentryEnvironment,
		FlushTimeout:      DefaultFlushTimeout,
	}
	config.SentryDSN = valueOrDefault(lookup, envSentryDSN, BuildSentryDSN)
	config.SentryEnvironment = valueOrDefault(lookup, envSentryEnvironment, config.SentryEnvironment)
	config.SentryRelease = value(lookup, envSentryRelease)

	if telemetryMode := strings.ToLower(value(lookup, envTelemetry)); telemetryMode == "disabled" {
		return config.normalized()
	} else if telemetryMode == "enabled" {
		config.Enabled = true
	} else {
		config.Enabled = config.SentryDSN != ""
	}
	return config.normalized()
}

func (c Config) normalized() Config {
	c.SentryDSN = strings.TrimSpace(c.SentryDSN)
	c.SentryEnvironment = strings.TrimSpace(c.SentryEnvironment)
	if c.SentryEnvironment == "" {
		c.SentryEnvironment = defaultSentryEnvironment
	}
	c.SentryRelease = strings.TrimSpace(c.SentryRelease)
	if c.FlushTimeout <= 0 || c.FlushTimeout > DefaultFlushTimeout {
		c.FlushTimeout = DefaultFlushTimeout
	}
	return c
}

func value(lookup func(string) (string, bool), key string) string {
	value, ok := lookup(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func valueOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	if value := value(lookup, key); value != "" {
		return value
	}
	return fallback
}

func validSentryDSN(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User == nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return false
	}
	username := parsed.User.Username()
	if username == "" || strings.ContainsAny(username, "\r\n\t") {
		return false
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return false
	}
	return strings.Trim(parsed.Path, "/") != ""
}
