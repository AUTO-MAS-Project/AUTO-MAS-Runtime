package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

const (
	maxDarwinTreeDepth   = 256
	maxDarwinTreeEntries = 1_000_000
)

type darwinTreeProof struct {
	name      string
	identity  DirectoryIdentity
	directory bool
	children  []darwinTreeProof
}

type darwinRemoval struct {
	count       int
	rootRemoved bool
}

func scanDarwinTree(ctx context.Context, directory *darwinDirectory, depth int, count *int) (proof darwinTreeProof, resultErr error) {
	if depth > maxDarwinTreeDepth || *count > maxDarwinTreeEntries {
		return darwinTreeProof{}, fmt.Errorf("%w: directory scan exceeds validated limits", ErrIdentityChanged)
	}
	if err := ctx.Err(); err != nil {
		return darwinTreeProof{}, err
	}
	reader, err := directory.openDirectory(ctx, ".", false)
	if err != nil {
		return darwinTreeProof{}, err
	}
	defer func() { resultErr = joinOperationCleanup(resultErr, reader.file.Close()) }()
	proof = darwinTreeProof{identity: directory.identity, directory: true}
	for {
		// 逐批读取，避免深层目录同时保留多个无上限的条目切片。
		entries, readErr := reader.file.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return darwinTreeProof{}, readErr
		}
		for _, entry := range entries {
			*count += 1
			if *count > maxDarwinTreeEntries {
				return darwinTreeProof{}, fmt.Errorf("%w: directory scan exceeds validated limits", ErrIdentityChanged)
			}
			childProof, err := scanDarwinEntry(ctx, directory, entry.Name(), depth, count)
			if err != nil {
				return darwinTreeProof{}, err
			}
			proof.children = append(proof.children, childProof)
		}
		if errors.Is(readErr, io.EOF) || len(entries) == 0 {
			break
		}
	}
	sort.Slice(proof.children, func(i, j int) bool { return proof.children[i].name < proof.children[j].name })
	return proof, nil
}

func scanDarwinEntry(ctx context.Context, directory *darwinDirectory, name string, depth int, count *int) (darwinTreeProof, error) {
	if err := ctx.Err(); err != nil {
		return darwinTreeProof{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return darwinTreeProof{}, err
	}
	identity := darwinIdentity(stat)
	if identity.volumeSerial != directory.identity.volumeSerial {
		return darwinTreeProof{}, ErrIdentityChanged
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := validateDarwinOrdinaryFile(stat); err != nil {
			return darwinTreeProof{}, err
		}
		return darwinTreeProof{name: name, identity: identity}, nil
	}
	child, err := directory.openDirectory(ctx, name, false)
	if err != nil {
		return darwinTreeProof{}, err
	}
	if !identity.Equal(&child.identity) {
		return darwinTreeProof{}, errors.Join(ErrIdentityChanged, child.file.Close())
	}
	proof, scanErr := scanDarwinTree(ctx, child, depth+1, count)
	proof.name = name
	return proof, joinOperationCleanup(scanErr, child.file.Close())
}

func removeDarwinChildren(ctx context.Context, directory *darwinDirectory, proof darwinTreeProof, state *darwinRemoval) (resultErr error) {
	initial := state.count
	defer func() {
		if state.count != initial {
			resultErr = errors.Join(resultErr, directory.file.Sync())
		}
	}()
	if !directory.identity.Equal(&proof.identity) {
		return ErrIdentityChanged
	}
	for _, childProof := range proof.children {
		if err := ctx.Err(); err != nil {
			return err
		}
		if childProof.directory {
			child, err := directory.openDirectory(ctx, childProof.name, false)
			if err != nil {
				return err
			}
			removeErr := removeDarwinChildren(ctx, child, childProof, state)
			if removeErr == nil {
				removeErr = directory.verifyName(childProof.name, childProof.identity, true)
			}
			if removeErr == nil {
				removeErr = ctx.Err()
			}
			if removeErr == nil {
				removeErr = unix.Unlinkat(int(directory.file.Fd()), childProof.name, unix.AT_REMOVEDIR)
				if removeErr == nil {
					state.count++
				}
			}
			if err := errors.Join(removeErr, child.file.Close()); err != nil {
				return err
			}
			continue
		}
		file, identity, err := directory.openFile(ctx, childProof.name, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		if !identity.Equal(&childProof.identity) {
			return errors.Join(ErrIdentityChanged, file.Close())
		}
		removeErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if removeErr == nil {
			removeErr = ctx.Err()
		}
		if removeErr == nil {
			removeErr = directory.removeFile(childProof.name, file, identity)
			if removeErr == nil {
				state.count++
			}
		}
		if err := errors.Join(removeErr, file.Close()); err != nil {
			return err
		}
	}
	return nil
}
