package uv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type tarExtractor struct{}

func (tarExtractor) Extract(ctx context.Context, archivePath, stagingDir string) (returnErr error) {
	if ctx == nil {
		return errors.New("uv extraction context is nil")
	}
	input, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	compressed, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, compressed.Close()) }()
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	archive := tar.NewReader(compressed)
	seen := make(map[string]bool)
	var total int64
	found := false
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if count >= maxUVArchiveFiles || entry.Size < 0 || entry.Size > maxUVArchiveBytes || total > maxUVArchiveBytes-entry.Size {
			return errors.New("uv archive limits exceeded")
		}
		total += entry.Size
		name := strings.TrimSuffix(entry.Name, "/")
		parts := strings.Split(name, "/")
		if len(parts) < 1 || len(parts) > 2 || !strings.HasPrefix(parts[0], "uv-") || path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") || hasParentSegment(name) {
			return errors.New("uv archive entry path is unsafe")
		}
		if seen[name] {
			return errors.New("uv archive entry is duplicated")
		}
		seen[name] = true
		if entry.Typeflag == tar.TypeDir && len(parts) == 1 {
			continue
		}
		if len(parts) != 2 || entry.Typeflag != tar.TypeReg || (parts[1] != "uv" && parts[1] != "uvx") {
			return errors.New("uv archive entry is unexpected")
		}
		if parts[1] != "uv" {
			continue
		}
		file, err := root.OpenFile(config.UVExecutableName(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, contextReader{ctx: ctx, reader: archive})
		err = errors.Join(copyErr, file.Sync(), file.Close())
		if err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("uv archive does not contain uv")
	}
	return nil
}
