//go:build !windows && !darwin

package doctor

import (
	"context"
	"errors"
)

var errPlatformProbeUnsupported = errors.New("volume probe is unsupported on this platform")

func probeDiskFree(context.Context, string) (uint64, error)   { return 0, errPlatformProbeUnsupported }
func probeFileSystem(context.Context, string) (string, error) { return "", errPlatformProbeUnsupported }
