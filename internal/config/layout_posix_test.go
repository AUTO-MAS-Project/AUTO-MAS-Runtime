//go:build linux || darwin

package config_test

import (
	"errors"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestPOSIXLayout_RootAndCase(t *testing.T) {
	if _, err := config.NewLayout("/", "/"); !errors.Is(err, config.ErrAppRootIsRoot) {
		t.Fatalf("got %v, want root rejected", err)
	}
	a, err := config.NewLayout("/tmp/MAS", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.NewLayout("/tmp/mas", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if a.IdentityKey() == b.IdentityKey() {
		t.Fatal("got folded case, want distinct lexical identity")
	}
}

func TestLayout_POSIXIdentity(t *testing.T) {
	if _, err := config.NewLayout("/", "/tmp"); !errors.Is(err, config.ErrAppRootIsRoot) {
		t.Fatalf("got %v, want ErrAppRootIsRoot", err)
	}
	upper, err := config.NewLayout("/tmp/AUTO-MAS", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	lower, err := config.NewLayout("/tmp/auto-mas", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if upper.IdentityKey() == lower.IdentityKey() || upper.IdentityKey() != "/tmp/AUTO-MAS" {
		t.Fatalf("got %q and %q, want distinct POSIX lexical keys", upper.IdentityKey(), lower.IdentityKey())
	}
}
