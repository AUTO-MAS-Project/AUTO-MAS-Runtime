package gitrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestGoGit_HTTPRedirectDoesNotForwardCredentials(t *testing.T) {
	for _, test := range []struct {
		name        string
		crossOrigin bool
	}{
		{name: "same origin retains authentication"},
		{name: "different port strips authentication", crossOrigin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := make(chan string, 1)
			advertise := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case seen <- r.Header.Get("Authorization"):
				default:
					t.Error("got duplicate redirected discovery, want one request")
				}
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				const hash = "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"
				for _, line := range []string{
					"# service=git-upload-pack\n", "", hash + " HEAD\x00multi_ack\n", hash + " refs/heads/main\n", "",
				} {
					packet := "0000"
					if line != "" {
						packet = fmt.Sprintf("%04x%s", len(line)+4, line)
					}
					if _, err := fmt.Fprint(w, packet); err != nil {
						t.Errorf("write advertisement error = %v", err)
						return
					}
				}
			})
			destination := httptest.NewTLSServer(advertise)
			t.Cleanup(destination.Close)
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected.git/info/refs" {
					advertise.ServeHTTP(w, r)
					return
				}
				if _, _, ok := r.BasicAuth(); !ok {
					t.Error("got unauthenticated initial request, want fixture credentials")
				}
				target := "/redirected.git/info/refs?service=git-upload-pack"
				if test.crossOrigin {
					target = destination.URL + target
				}
				http.Redirect(w, r, target, http.StatusFound)
			}))
			t.Cleanup(origin.Close)
			endpoint, err := transport.NewEndpoint(origin.URL + "/repo.git")
			if err != nil {
				t.Fatal(err)
			}
			client := origin.Client()
			t.Cleanup(client.CloseIdleConnections)
			auth := &githttp.BasicAuth{Username: "fixture", Password: "fixture-password"}
			session, err := githttp.NewClient(client).NewUploadPackSession(endpoint, auth)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Errorf("close session error = %v", err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			refs, err := session.AdvertisedReferencesContext(ctx)
			if err != nil {
				t.Fatalf("advertised references error = %v", err)
			}
			if _, ok := refs.References["refs/heads/main"]; !ok {
				t.Fatal("got no main reference, want successful redirected discovery")
			}
			select {
			case header := <-seen:
				wantAuth := !test.crossOrigin
				if gotAuth := header != ""; gotAuth != wantAuth {
					t.Errorf("redirect authentication present = %t, want %t", gotAuth, wantAuth)
				}
			default:
				t.Fatal("got no destination request, want redirected discovery")
			}
		})
	}
}

func TestGoGitCloneOptionsContract(t *testing.T) {
	branch := plumbing.NewBranchReferenceName("release/v5.3.1")
	options := git.CloneOptions{
		URL:           "https://example.com/AUTO-MAS.git",
		ReferenceName: branch,
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("CloneOptions.Validate() error = %v", err)
	}
	if options.ReferenceName != branch {
		t.Fatalf("ReferenceName = %q, want %q", options.ReferenceName, branch)
	}
	if !options.SingleBranch || options.Depth != 1 || options.Tags != git.NoTags {
		t.Fatalf(
			"clone shape = singleBranch:%t depth:%d tags:%d, want true/1/NoTags",
			options.SingleBranch,
			options.Depth,
			options.Tags,
		)
	}
	if options.InsecureSkipTLS {
		t.Fatal("InsecureSkipTLS = true, want false")
	}
	if options.RecurseSubmodules != git.NoRecurseSubmodules {
		t.Fatalf("RecurseSubmodules = %d, want disabled", options.RecurseSubmodules)
	}
}
