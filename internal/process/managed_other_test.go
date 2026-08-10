//go:build !windows

package process

import (
	"errors"
	"testing"
)

func TestJob_UnsupportedPlatformFailsClosed(t *testing.T) {
	managed, err := StartManaged(t.Context(), StartSpec{})
	if managed != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("StartManaged() = %#v, %v", managed, err)
	}
}
