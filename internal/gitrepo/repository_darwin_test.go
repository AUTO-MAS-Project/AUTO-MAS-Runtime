package gitrepo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinRepository_RejectsExternalGitDirectory(t *testing.T) {
	layout := mustGitLayout(t)
	outside, _ := createVerifiedRepository(t, "v5.4.0", recoverySourceURL(t))
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(outside, ".git")
	marker := filepath.Join(layout.RepoDir(), ".git")
	if err := os.WriteFile(marker, []byte("gitdir: "+gitdir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || result.Reason != "invalid" {
		t.Fatalf("Check(external git directory) = %#v, want invalid repository", result)
	}
}

func TestDarwinRepository_RejectsAlternateObjectStore(t *testing.T) {
	layout := mustGitLayout(t)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "active")
	service, err := NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := service.Check(t.Context())
	if err != nil || !healthy.Healthy {
		t.Fatalf("Check(self-contained) = %#v, %v, want healthy", healthy, err)
	}
	outside, _ := createVerifiedRepository(t, "v5.4.0", recoverySourceURL(t))
	alternate := filepath.Join(layout.RepoDir(), ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alternate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alternate, []byte(filepath.Join(outside, ".git", "objects")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.Check(t.Context())
	if err != nil || result.Healthy || result.Reason != "invalid" {
		t.Fatalf("Check(alternates) = %#v, %v, want invalid", result, err)
	}
}
