package uv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestBootstrap_PublishClosesStagingHandlesBeforeRename(t *testing.T) {
	layout := newUVTestLayout(t)
	archive := makeUVArchive(t)
	artifact := testArtifact(string(archive))
	bootstrapper := newTestBootstrapper(
		t,
		layout,
		artifact,
		&fakeDownloader{payload: archive},
		&fakeVersionChecker{},
		zipExtractor{},
		nil,
	)

	executable, err := bootstrapper.Ensure(t.Context(), testOperationID, testMirrorPolicy(t))
	if err != nil {
		var operationErr *Error
		if errors.As(err, &operationErr) && operationErr.Code() == protocol.CodeDirectoryOccupied {
			t.Fatalf("Ensure() error = %v, bootstrap retained a conflicting staging handle", err)
		}
		t.Fatalf("Ensure() error = %v", err)
	}
	wantExecutable, err := layout.UVExecutable(artifact.Version)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if executable != wantExecutable {
		t.Fatalf("Ensure() executable = %q, want %q", executable, wantExecutable)
	}
	if info, err := os.Lstat(executable); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published uv.exe = %#v, %v, want regular file", info, err)
	}
	staging, err := layout.UVStagingDir(artifact.Version, testOperationID)
	if err != nil {
		t.Fatalf("UVStagingDir() error = %v", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging path %q error = %v, want absent after rename", filepath.Clean(staging), err)
	}
	archivePath := mustDownloadPath(t, layout, artifact.Name)
	movedArchive := archivePath + ".closed"
	if err := os.Rename(archivePath, movedArchive); err != nil {
		t.Fatalf("Rename(archive after publish) error = %v, want all ZIP/cache handles closed", err)
	}
}
