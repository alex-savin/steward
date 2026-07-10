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
	"path/filepath"

	"github.com/alex-savin/steward/internal/execx"
)

// Sync brings dir to an exact commit: clones on first use, then fetches and
// force-checks-out the SHA (detached HEAD). Deploying by SHA rather than
// branch means the deployed tree is exactly what the webhook announced, and
// rollbacks can target any previously-deployed commit.
func Sync(ctx context.Context, log *slog.Logger, cloneURL, dir, sha string) error {
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
	if _, err := execx.Run(ctx, dir, nil, "git", "checkout", "--force", "--detach", sha); err != nil {
		// A force-push race can leave the SHA unreachable from branch heads;
		// try fetching the commit directly before giving up.
		if _, ferr := execx.Run(ctx, dir, nil, "git", "fetch", "--force", "origin", sha); ferr != nil {
			return fmt.Errorf("git checkout %s: %w", sha, err)
		}
		if _, err := execx.Run(ctx, dir, nil, "git", "checkout", "--force", "--detach", sha); err != nil {
			return fmt.Errorf("git checkout %s: %w", sha, err)
		}
	}
	return nil
}
