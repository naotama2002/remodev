package session

import (
	"context"

	"github.com/naotama2002/remodev/internal/github"
)

// GitHub is what the session manager needs from GitHub. *github.Client
// implements it; tests use internal/fake.
type GitHub interface {
	Repo(ctx context.Context, fullName string) (*github.Repo, error)
	FindPR(ctx context.Context, fullName, branch string) (*github.PR, error)
	CheckRuns(ctx context.Context, repo, ref string) ([]github.CheckRun, error)
	CompareDiff(ctx context.Context, repo, base, head string) (string, error)
	Dir(ctx context.Context, repo, dir, ref string) ([]github.File, error)
}

var _ GitHub = (*github.Client)(nil)
