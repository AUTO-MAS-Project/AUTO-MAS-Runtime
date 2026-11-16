package filesystem

import "golang.org/x/sys/unix"

func renamePOSIXNoReplace(a int, source string, b int, destination string) error {
	return unix.Renameat2(a, source, b, destination, unix.RENAME_NOREPLACE)
}
