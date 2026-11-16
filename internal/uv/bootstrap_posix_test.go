//go:build linux || darwin

package uv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

func TestPOSIXBootstrap_RoundTrip(t *testing.T) {
	layout := newUVTestLayout(t)
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	payload := []byte("#!/bin/sh\nprintf 'uv " + FixedVersion + "\\n'\n")
	if err := archive.WriteHeader(&tar.Header{Name: "uv-fixture/uv", Mode: 0700, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(buffer.Bytes())
	artifact := Artifact{Version: FixedVersion, Kind: mirror.KindUV, Name: "uv-posix-test.tar.gz", SHA256: fmt.Sprintf("%x", hash)}
	downloader := &fakeDownloader{payload: buffer.Bytes(), path: mustDownloadPath(t, layout, artifact.Name)}
	bootstrapper, err := NewBootstrapper(layout, WithArtifact(artifact), WithDownloader(downloader))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	if err != nil {
		t.Fatalf("bootstrap %v: %v", err, errors.Unwrap(err))
	}
	info, err := os.Stat(executable)
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatalf("executable permission: %v %v", info, err)
	}
	calls := downloader.calls
	if _, err = bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t)); err != nil {
		t.Fatal(err)
	}
	if downloader.calls != calls {
		t.Fatal("cache hit downloaded artifact again")
	}
}
