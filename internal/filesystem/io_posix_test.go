//go:build linux || darwin

package filesystem_test

import (
	"os"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestPOSIXIO_DownloadAndLog(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	downloads, err := filesystem.NewDownloadFiles(layout)
	if err != nil {
		t.Fatal(err)
	}
	session, err := downloads.Begin(t.Context(), "asset.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if result, err := session.PublishNoReplace(t.Context()); err != nil || !result.Published {
		t.Fatalf("got %+v %v", result, err)
	}
	if data, err := os.ReadFile(session.Path()); err != nil || string(data) != "payload" {
		t.Fatalf("got %q %v", data, err)
	}
	logs, err := filesystem.NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := logs.OpenAppend(t.Context(), "doctor", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := logs.List(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatalf("got %d logs, %v", len(entries), err)
	}
	if result, err := logs.Remove(t.Context(), entries[0]); err != nil || !result.MutationApplied {
		t.Fatalf("got %+v %v", result, err)
	}
	if err = logs.Close(); err != nil {
		t.Fatal(err)
	}
}
