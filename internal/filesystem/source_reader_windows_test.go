//go:build windows

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func sourceReaderFixture(t *testing.T) (*config.Layout, string, string) {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(layout.RepoDir(), "a", "first.py"), filepath.Join(layout.RepoDir(), "b", "second.py")
	for _, path := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return layout, a, b
}

func TestManagedSourceReader_PinsAndReleasesSharedAncestors(t *testing.T) {
	layout, a, b := sourceReaderFixture(t)
	reader, err := NewManagedSourceReader(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, path := range []string{a, a, b} {
		got, err := reader.Read(t.Context(), path, 20)
		if err != nil || string(got) != "original" {
			t.Fatalf("read %s = %q, %v", path, got, err)
		}
		parent := filepath.Dir(path)
		if err := os.Rename(parent, parent+"-moved"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("cached ancestor replacement = %v, want sharing violation", err)
		}
	}
	// 转到 b 后，a 已不再属于公共链，应立即释放。
	if err := os.Rename(filepath.Dir(a), filepath.Dir(a)+"-moved"); err != nil {
		t.Fatalf("old suffix still pinned: %v", err)
	}
	if err := os.Rename(layout.RepoDir(), layout.RepoDir()+"-moved"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("repo replacement = %v, want sharing violation", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(t.Context(), b, 20); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if err := os.Rename(layout.RepoDir(), layout.RepoDir()+"-moved"); err != nil {
		t.Fatalf("repo still pinned after close: %v", err)
	}
}

func TestManagedSourceReader_RechecksCachedDirectory(t *testing.T) {
	for _, kind := range []string{"identity", "reparse", "case", "case_query_error"} {
		t.Run(kind, func(t *testing.T) {
			layout, a, _ := sourceReaderFixture(t)
			api := newProductionPathAPI()
			identity, caseSensitive := api.identity, api.caseSensitive
			var changed windows.Handle
			api.identity = func(handle windows.Handle) (objectIdentity, error) {
				got, err := identity(handle)
				if handle == changed {
					if kind == "identity" {
						got.fileID[0] ^= 0xff
					}
					if kind == "reparse" {
						got.attributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
					}
				}
				return got, err
			}
			api.caseSensitive = func(handle windows.Handle) (bool, error) {
				if handle == changed && kind == "case" {
					return true, nil
				}
				if handle == changed && kind == "case_query_error" {
					return false, errors.New("injected case query failure")
				}
				return caseSensitive(handle)
			}
			reader, err := newManagedSourceReaderWithAPI(t.Context(), layout, api)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := reader.Read(t.Context(), a, 20); err != nil {
				t.Fatal(err)
			}
			changed = reader.chain.objects[len(reader.chain.objects)-1].handle
			_, err = reader.Read(t.Context(), a, 20)
			switch kind {
			case "identity":
				if !errors.Is(err, ErrIdentityChanged) {
					t.Fatalf("identity change = %v", err)
				}
			case "reparse":
				assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
			default:
				if !errors.Is(err, ErrUnsupportedCaseSensitivity) {
					t.Fatalf("case change = %v", err)
				}
			}
		})
	}
}

func TestManagedSourceReader_Boundaries(t *testing.T) {
	layout, a, _ := sourceReaderFixture(t)
	reader, err := NewManagedSourceReader(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := reader.Read(t.Context(), filepath.Join(t.TempDir(), "outside"), 20); err == nil {
		t.Fatal("accepted outside path")
	}
	if _, err := reader.Read(t.Context(), filepath.Dir(a), 20); err == nil {
		t.Fatal("accepted directory as file")
	}
	if _, err := reader.Read(t.Context(), filepath.Join(filepath.Dir(a), "missing"), 20); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Read(ctx, a, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	got, err := reader.Read(t.Context(), a, 4)
	if err != nil || string(got) != "orig" {
		t.Fatalf("bounded read = %q, %v", got, err)
	}
}

func TestManagedSourceReader_CloseFailure(t *testing.T) {
	layout, a, _ := sourceReaderFixture(t)
	api := newProductionPathAPI()
	closeHandle := api.closeHandle
	var failed windows.Handle
	api.closeHandle = func(handle windows.Handle) error {
		if handle == failed {
			failed = 0
			return errors.New("injected close failure")
		}
		return closeHandle(handle)
	}
	reader, err := newManagedSourceReaderWithAPI(t.Context(), layout, api)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := reader.Read(t.Context(), a, 20); err != nil {
		t.Fatal(err)
	}
	failed = reader.chain.objects[len(reader.chain.objects)-1].handle
	if err := reader.Close(); err == nil {
		t.Fatal("close error was hidden")
	}
	if _, err := reader.Read(t.Context(), a, 20); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after partial close = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(layout.RepoDir(), layout.RepoDir()+"-moved"); err != nil {
		t.Fatalf("leaked handle: %v", err)
	}
}

func TestManagedSourceReader_BlocksConcurrentWriter(t *testing.T) {
	layout, a, _ := sourceReaderFixture(t)
	api := newProductionPathAPI()
	readFile := api.readFile
	name, err := windows.UTF16PtrFromString(a)
	if err != nil {
		t.Fatal(err)
	}
	attempted := false
	api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		attempted = true
		writer, openErr := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
		if openErr == nil {
			_ = windows.CloseHandle(writer)
			return 0, errors.New("concurrent writer was allowed")
		}
		if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) {
			return 0, openErr
		}
		return readFile(handle, buffer)
	}
	reader, err := newManagedSourceReaderWithAPI(t.Context(), layout, api)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := reader.Read(t.Context(), a, 20)
	if err != nil || string(got) != "original" || !attempted {
		t.Fatalf("read = %q, %v, attempted=%t", got, err, attempted)
	}
}

func BenchmarkManagedSourceReader(b *testing.B) {
	root := b.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		b.Fatal(err)
	}
	paths := make([]string, 0, 1024)
	payload := make([]byte, 1024)
	for directory := range 32 {
		parent := filepath.Join(layout.RepoDir(), fmt.Sprintf("dir-%02d", directory))
		if err := os.MkdirAll(parent, 0o700); err != nil {
			b.Fatal(err)
		}
		for file := range 32 {
			path := filepath.Join(parent, fmt.Sprintf("file-%02d.py", file))
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				b.Fatal(err)
			}
			paths = append(paths, path)
		}
	}
	for _, reuse := range []bool{false, true} {
		b.Run(map[bool]string{false: "full_chain", true: "shared_chain"}[reuse], func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				read := func(ctx context.Context, path string, limit int64) ([]byte, error) {
					return ReadManagedFile(ctx, layout, path, limit)
				}
				var reader *ManagedSourceReader
				if reuse {
					var err error
					reader, err = NewManagedSourceReader(b.Context(), layout)
					if err != nil {
						b.Fatal(err)
					}
					read = reader.Read
				}
				for _, path := range paths {
					data, err := read(b.Context(), path, int64(len(payload)+1))
					if err != nil || len(data) != len(payload) {
						if reader != nil {
							err = errors.Join(err, reader.Close())
						}
						b.Fatalf("source read = %d bytes, %v", len(data), err)
					}
				}
				if reader != nil {
					if err := reader.Close(); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
