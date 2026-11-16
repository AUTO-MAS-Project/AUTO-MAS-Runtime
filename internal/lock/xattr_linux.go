package lock

import (
	"errors"

	"golang.org/x/sys/unix"
)

func missingPOSIXXattr(err error) bool { return errors.Is(err, unix.ENODATA) }
