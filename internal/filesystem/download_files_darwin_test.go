package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinDownload_PublishAndAbort(t *testing.T) {
	layout := darwinTestLayout(t)
	files, err := NewDownloadFiles(layout)
	if err != nil {
		t.Fatalf("NewDownloadFiles() = %v, want nil", err)
	}
	session, err := files.Begin(t.Context(), "payload.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := session.Abort(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if n, err := session.Write([]byte("verified payload")); err != nil || n != 16 {
		t.Fatalf("Write() = %d, %v, want 16, nil", n, err)
	}
	before, err := os.Stat(session.PartPath())
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.PublishNoReplace(t.Context())
	if err != nil || !result.Published {
		t.Fatalf("PublishNoReplace() = %#v, %v, want published", result, err)
	}
	after, err := os.Stat(session.Path())
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("published identity = %v, %v, want original part", after, err)
	}
	if result, err := session.Abort(t.Context()); err != nil || result.Removed {
		t.Fatalf("Abort(published) = %#v, %v, want unchanged", result, err)
	}
	if result, err := session.PublishNoReplace(t.Context()); err != nil || !result.Published {
		t.Fatalf("PublishNoReplace twice = %#v, %v, want published", result, err)
	}
	if _, err := session.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write(published) = %v, want ErrClosed", err)
	}
}

func TestDarwinDownload_CompetitionAndReplacement(t *testing.T) {
	for _, scenario := range []string{"destination", "part replacement"} {
		t.Run(scenario, func(t *testing.T) {
			layout := darwinTestLayout(t)
			files, err := NewDownloadFiles(layout)
			if err != nil {
				t.Fatal(err)
			}
			session, err := files.Begin(t.Context(), "payload")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, err := session.Abort(context.Background())
				if err != nil && !errors.Is(err, ErrIdentityChanged) {
					t.Error(err)
				}
			})
			if _, err := session.Write([]byte("owned")); err != nil {
				t.Fatal(err)
			}
			var competing string
			if scenario == "destination" {
				competing = session.Path()
			} else {
				if err := os.Rename(session.PartPath(), session.PartPath()+"-saved"); err != nil {
					t.Fatal(err)
				}
				competing = session.PartPath()
			}
			if err := os.WriteFile(competing, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, publishErr := session.PublishNoReplace(t.Context())
			if publishErr == nil || result.Published {
				t.Fatalf("PublishNoReplace() = %#v, %v, want rejection", result, publishErr)
			}
			_, abortErr := session.Abort(t.Context())
			if scenario == "destination" && abortErr != nil {
				t.Fatal(abortErr)
			}
			if scenario == "part replacement" && !errors.Is(abortErr, ErrIdentityChanged) {
				t.Fatalf("Abort(replaced) = %v, want ErrIdentityChanged", abortErr)
			}
			data, err := os.ReadFile(competing)
			if err != nil || string(data) != "foreign" {
				t.Fatalf("foreign payload = %q, %v, want preserved", data, err)
			}
		})
	}
}

func TestDarwinDownload_RejectsUnsafePart(t *testing.T) {
	layout := darwinTestLayout(t)
	if err := os.MkdirAll(layout.DownloadCacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(outside, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	part, err := layout.DownloadPartFile("payload")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, part); err != nil {
		t.Fatal(err)
	}
	files, err := NewDownloadFiles(layout)
	if err != nil {
		t.Fatal(err)
	}
	if session, err := files.Begin(t.Context(), "payload"); err == nil || session != nil {
		t.Fatalf("Begin(symlink part) = %v, %v, want rejection", session, err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("sentinel = %q, %v, want preserved", data, err)
	}
}

func TestDarwinDownload_ActiveSessionIsNotStale(t *testing.T) {
	layout := darwinTestLayout(t)
	files, err := NewDownloadFiles(layout)
	if err != nil {
		t.Fatal(err)
	}
	active, err := files.Begin(t.Context(), "active")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := active.Abort(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if second, err := files.Begin(t.Context(), "active"); err == nil || second != nil {
		t.Fatalf("Begin(active part) = %v, %v, want conflict", second, err)
	}
	if _, err := active.Write([]byte("still owned")); err != nil {
		t.Fatalf("Write after competitor = %v, want original session preserved", err)
	}
	other, err := files.Begin(t.Context(), "independent")
	if err != nil {
		t.Fatalf("Begin(independent) = %v, want concurrent different name", err)
	}
	if _, err := other.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}
