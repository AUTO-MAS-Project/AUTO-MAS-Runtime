//go:build darwin

package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// replaceDarwin 以同目录独占临时文件、文件同步和 rename 提交状态替换。
//
// 提交前复核目录身份，失败时只清理本次创建的临时文件。
func replaceDarwin(ctx context.Context, directory *darwinDirectory, name string, data []byte) (bool, error) {
	if err := verifyDarwinStateDirectory(directory, directory.identity); err != nil {
		return false, err
	}
	// 先以只读方式打开已有对象，拒绝对链接或硬链接产生副作用。
	existing, _, err := directory.openFile(ctx, name, os.O_RDONLY, 0)
	if err == nil {
		err = existing.Close()
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return false, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return false, err
	}
	temp := ".runtime-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, _, err := directory.openFile(ctx, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err == nil {
		err = verifyDarwinStateDirectory(directory, directory.identity)
	}
	if err == nil {
		err = unix.Renameat(int(directory.file.Fd()), temp, int(directory.file.Fd()), name)
	}
	if err != nil {
		return false, errors.Join(err, unix.Unlinkat(int(directory.file.Fd()), temp, 0))
	}
	return true, directory.file.Sync()
}
