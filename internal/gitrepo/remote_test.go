package gitrepo

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestRemoteCheck_ReadOnlyAndCommitComparison(t *testing.T) {
	s, layout, _, _ := stagingFixture(t)
	current, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{current.Commit, testGitCommit} {
		s.resolveRemote = func(context.Context, mirror.Plan, Target) (string, error) { return commit, nil }
		policy, err := mirror.NewPolicy(mirror.PolicySpec{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.CheckRemote(t.Context(), policy)
		if err != nil || result.UpdateAvailable != (commit != current.Commit) || result.RemoteCommit != commit {
			t.Fatalf("CheckRemote = %+v, %v", result, err)
		}
		for _, path := range []string{layout.UpdateStateFile(), layout.MutationStateFile()} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("check wrote %s: %v", path, err)
			}
		}
	}
}

func TestRemoteResolve_MirrorFallbackAndFailures(t *testing.T) {
	for _, name := range []string{"fallback", "missing", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			target := mustParseTarget(t, "v1.0.0")
			client := &fakeGitClient{list: func(ctx context.Context, _ string, _ []byte) ([]*plumbing.Reference, error) {
				calls++
				if name == "cancelled" {
					cancel()
					return nil, ctx.Err()
				}
				if name == "missing" {
					return nil, nil
				}
				if calls == 1 {
					return nil, errors.New("source unavailable")
				}
				return []*plumbing.Reference{plumbing.NewHashReference(plumbing.NewBranchReferenceName(target.Branch()), plumbing.NewHash(testGitCommit))}, nil
			}}
			commit, err := resolveRemoteWithClient(ctx, mustGitPlan(t, "cnb"), target, client)
			if name == "fallback" {
				if err != nil || commit != testGitCommit || calls < 2 {
					t.Fatalf("resolve = %s, %v, calls=%d", commit, err, calls)
				}
				return
			}
			var coded interface{ Code() protocol.Code }
			want := protocol.CodeGitBranchNotFound
			if name == "cancelled" {
				want = protocol.CodeOperationCancelled
			}
			if !errors.As(err, &coded) || coded.Code() != want {
				t.Fatalf("resolve = %v, want %s", err, want)
			}
		})
	}
}
