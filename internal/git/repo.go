// Package git manages the working checkouts Steward deploys from,
// shelling out to the system git binary.
package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/alex-savin/steward/internal/execx"
)

// ErrNotOnBranch is returned by Sync when the requested commit is not on the
// project's branch. The checkout is left untouched.
var ErrNotOnBranch = errors.New("commit is not on the deployed branch")

// shaPattern accepts hex object names only (SHA-1 or SHA-256, abbreviated or
// full). The SHA comes from the webhook payload; anything else — a branch
// name, a "--option" — must never reach git's argument list.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// Sync brings dir to an exact commit: clones on first use, then fetches and
// force-checks-out the SHA (detached HEAD). Deploying by SHA rather than
// branch means the deployed tree is exactly what the webhook announced, and
// rollbacks can target any previously-deployed commit.
//
// When branch is non-empty, the SHA must be an ancestor of (or equal to)
// origin/<branch> after the fetch, or Sync returns ErrNotOnBranch BEFORE the
// checkout: the webhook secret alone must not be enough to deploy (and run the
// manifest commands of) an arbitrary commit that merely exists on the origin.
// The working tree stays at the previous deploy, so a later image-only deploy
// still reads a vetted manifest. Rollback passes "" — its target was on the
// branch when it was deployed, and a force-push since must not block it.
func Sync(ctx context.Context, log *slog.Logger, cloneURL, dir, sha, branch string) error {
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("invalid commit SHA %q", sha)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		log.Info("cloning repository", "url", cloneURL, "dir", dir)
		if _, err := execx.Run(ctx, "", nil, "git", "clone", "--no-checkout", cloneURL, dir); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	}

	if _, err := execx.Run(ctx, dir, nil, "git", "fetch", "--force", "--tags", "origin"); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if _, err := execx.Run(ctx, dir, nil, "git", "cat-file", "-e", sha+"^{commit}"); err != nil {
		// A force-push race can leave the SHA unreachable from branch heads;
		// try fetching the commit directly before giving up.
		if _, ferr := execx.Run(ctx, dir, nil, "git", "fetch", "--force", "origin", sha); ferr != nil {
			return fmt.Errorf("git fetch %s: commit not found on origin: %w", sha, ferr)
		}
	}
	if branch != "" {
		if err := checkOnBranch(ctx, dir, sha, branch); err != nil {
			return err
		}
	}
	if _, err := execx.Run(ctx, dir, nil, "git", "checkout", "--force", "--detach", sha); err != nil {
		return fmt.Errorf("git checkout %s: %w", sha, err)
	}
	return nil
}

// checkOnBranch runs `git merge-base --is-ancestor`, which exits 0 when sha is
// reachable from the branch head, 1 when it is not, and >1 on error (such as
// the branch not existing on the origin).
func checkOnBranch(ctx context.Context, dir, sha, branch string) error {
	head := "refs/remotes/origin/" + branch
	_, err := execx.Run(ctx, dir, nil, "git", "merge-base", "--is-ancestor", sha, head)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return fmt.Errorf("%w: %s is not an ancestor of origin/%s", ErrNotOnBranch, sha, branch)
	default:
		return fmt.Errorf("check %s is on origin/%s: %w", sha, branch, err)
	}
}
