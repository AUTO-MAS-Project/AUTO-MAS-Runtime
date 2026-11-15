package filesystem

import "golang.org/x/sys/unix"

func renamePOSIXNoReplace(a int, source string, b int, destination string) error {
	return unix.RenameatxNp(a, source, b, destination, unix.RENAME_EXCL)
}
