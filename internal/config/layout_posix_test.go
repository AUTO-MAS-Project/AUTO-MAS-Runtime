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
