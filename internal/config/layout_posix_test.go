//go:build !windows

package config

import (
	"errors"
	"testing"
)

func TestLayout_POSIXIdentity(t *testing.T) {
	if _, err := NewLayout("/", "/tmp"); !errors.Is(err, ErrAppRootIsRoot) {
		t.Fatalf("got %v, want ErrAppRootIsRoot", err)
	}
	upper, err := NewLayout("/tmp/AUTO-MAS", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	lower, err := NewLayout("/tmp/auto-mas", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if upper.IdentityKey() == lower.IdentityKey() || upper.IdentityKey() != "/tmp/AUTO-MAS" {
		t.Fatalf("got %q and %q, want distinct POSIX lexical keys", upper.IdentityKey(), lower.IdentityKey())
	}
}
