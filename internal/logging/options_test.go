package logging

import (
	"errors"
	"testing"
	"time"
)

func TestLevel_StringAndValid(t *testing.T) {
	tests := []struct {
		name  string
		level Level
		valid bool
	}{
		{name: "debug", level: LevelDebug, valid: true},
		{name: "info", level: LevelInfo, valid: true},
		{name: "warn", level: LevelWarn, valid: true},
		{name: "error", level: LevelError, valid: true},
		{name: "zero", level: Level(""), valid: false},
		{name: "unknown", level: Level("trace"), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.level.String(); got != string(test.level) {
				t.Fatalf("String() = %q, want %q", got, test.level)
			}
			if got := test.level.Valid(); got != test.valid {
				t.Fatalf("Valid() = %v, want %v", got, test.valid)
			}
		})
	}
}

func TestDefaultRetentionPolicy_UsesThirtyDayAndFileLimits(t *testing.T) {
	got := DefaultRetentionPolicy()
	want := RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 30}
	if got != want {
		t.Fatalf("DefaultRetentionPolicy() = %#v, want %#v", got, want)
	}
}

func TestWithClock_RejectsNilAndPreservesFunction(t *testing.T) {
	_, err := applyOptions(WithClock(nil))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("applyOptions(WithClock(nil)) error = %v, want ErrInvalidArgument", err)
	}

	want := time.Date(2026, 7, 29, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	values, err := applyOptions(WithClock(func() time.Time { return want }))
	if err != nil {
		t.Fatalf("applyOptions(WithClock(valid)) error = %v", err)
	}
	if got := values.clock(); !got.Equal(want) || got.Location() != want.Location() {
		t.Fatalf("stored clock() = %v, want %v with the same location", got, want)
	}
}

func TestWithRetention_ValidatesAndCopiesPolicy(t *testing.T) {
	invalid := []RetentionPolicy{
		{MaxAgeDays: 0, MaxFilesPerCommand: 1},
		{MaxAgeDays: -1, MaxFilesPerCommand: 1},
		{MaxAgeDays: 1, MaxFilesPerCommand: 0},
		{MaxAgeDays: 1, MaxFilesPerCommand: -1},
	}
	for _, policy := range invalid {
		_, err := applyOptions(WithRetention(policy))
		if !errors.Is(err, ErrInvalidRetention) {
			t.Fatalf("applyOptions(WithRetention(%#v)) error = %v, want ErrInvalidRetention", policy, err)
		}
	}

	policy := RetentionPolicy{MaxAgeDays: 7, MaxFilesPerCommand: 9}
	values, err := applyOptions(WithRetention(policy))
	if err != nil {
		t.Fatalf("applyOptions(WithRetention(valid)) error = %v", err)
	}
	policy.MaxAgeDays = 99
	if got, want := values.retention, (RetentionPolicy{MaxAgeDays: 7, MaxFilesPerCommand: 9}); got != want {
		t.Fatalf("stored retention = %#v, want %#v", got, want)
	}
}

func TestOptions_RejectsNilOption(t *testing.T) {
	_, err := applyOptions(Option(nil))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("applyOptions(nil) error = %v, want ErrInvalidArgument", err)
	}
}
