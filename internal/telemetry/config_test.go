package telemetry

import (
	"testing"
	"time"
)

func TestConfig_DisabledBeforeClientConstruction(t *testing.T) {
	var calls int
	factory := func(Config) (provider, error) {
		calls++
		return nil, nil
	}
	config := Config{
		Enabled:   false,
		SentryDSN: "https://public@example.invalid/1",
	}

	observer := newObserverWithFactory(config, factory)
	observer.Close(contextForTest())
	if calls != 0 {
		t.Fatalf("provider factory calls = %d, want 0", calls)
	}
}

func TestConfig_OfflineBeforeClientConstruction(t *testing.T) {
	var calls int
	factory := func(Config) (provider, error) {
		calls++
		return nil, nil
	}
	config := Config{
		Enabled:   true,
		Offline:   true,
		SentryDSN: "https://public@example.invalid/1",
	}

	observer := newObserverWithFactory(config, factory)
	observer.Close(contextForTest())
	if calls != 0 {
		t.Fatalf("provider factory calls = %d, want 0", calls)
	}
}

func TestConfig_MissingCredentialsProduceNoOp(t *testing.T) {
	var calls int
	factory := func(Config) (provider, error) {
		calls++
		return nil, nil
	}
	config := Config{Enabled: true}

	observer := newObserverWithFactory(config, factory)
	observer.Close(contextForTest())
	if calls != 0 {
		t.Fatalf("provider factory calls = %d, want 0", calls)
	}
}

func TestConfig_EnvironmentOverridesBuildDefaults(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			envTelemetry:         "enabled",
			envSentryDSN:         "https://public@example.test/42",
			envSentryEnvironment: "staging",
			envSentryRelease:     "runtime@v1.2.3",
		}
		value, ok := values[key]
		return value, ok
	}

	config := LoadConfigFrom(lookup)
	if !config.Enabled || config.Offline {
		t.Fatalf("config gate = enabled=%v offline=%v, want enabled=true offline=false", config.Enabled, config.Offline)
	}
	if config.SentryDSN != "https://public@example.test/42" {
		t.Fatalf("SentryDSN = %q, want override", config.SentryDSN)
	}
	if config.SentryEnvironment != "staging" || config.SentryRelease != "runtime@v1.2.3" {
		t.Fatalf("Sentry metadata overrides were not retained: %+v", config)
	}
	if config.FlushTimeout != DefaultFlushTimeout {
		t.Fatalf("FlushTimeout = %s, want %s", config.FlushTimeout, DefaultFlushTimeout)
	}
}

func TestConfig_DefaultsAreStable(t *testing.T) {
	config := LoadConfigFrom(func(string) (string, bool) { return "", false })
	if config.FlushTimeout != 500*time.Millisecond {
		t.Fatalf("FlushTimeout = %s, want 500ms", config.FlushTimeout)
	}
	if config.Enabled {
		t.Fatal("empty credentials unexpectedly enabled telemetry")
	}
}
