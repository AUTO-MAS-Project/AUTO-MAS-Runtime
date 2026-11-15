//go:build linux || darwin

package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func replacePOSIX(ctx context.Context, c *posixChain, name string, data []byte) (bool, error) {
	if err := c.verify(ctx); err != nil {
		return false, err
	}
	old, err := openPOSIXFile(c, name, unix.O_RDONLY)
	if err == nil {
		err = old.Close()
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
	f, err := openPOSIXFile(c, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return false, err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = c.verify(ctx)
	}
	if err == nil {
		err = unix.Renameat(int(c.leaf().Fd()), temp, int(c.leaf().Fd()), name)
	}
	if err != nil {
		return false, errors.Join(err, unix.Unlinkat(int(c.leaf().Fd()), temp, 0))
	}
	return true, c.leaf().Sync()
}
