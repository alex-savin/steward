package git

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runGit runs git in dir with a fixed identity, so tests need no global config.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=test@steward.local",
		"-c", "user.name=Steward Test",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteRepo builds a bare origin with two commits on main and one commit on
// a side branch that is pushed to the origin but never merged into main.
func remoteRepo(t *testing.T) (origin, first, head, side string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	origin = t.TempDir()
	runGit(t, origin, "init", "--bare")

	work := t.TempDir()
	runGit(t, work, "init")
	runGit(t, work, "remote", "add", "origin", origin)
	commit := func(name string) string {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-m", name)
		return runGit(t, work, "rev-parse", "HEAD")
	}
	first = commit("one")
	head = commit("two")
	runGit(t, work, "push", "origin", "HEAD:main")

	runGit(t, work, "checkout", "-q", "-b", "side")
	side = commit("side")
	runGit(t, work, "push", "origin", "side")
	return origin, first, head, side
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "rev-parse", "HEAD")
}

// The branch head and any older commit on the branch deploy (an older one is
// how a manual rollback-by-dispatch works).
func TestSyncAllowsCommitsOnBranch(t *testing.T) {
	origin, first, head, _ := remoteRepo(t)
	dir := filepath.Join(t.TempDir(), "checkout")

	for _, sha := range []string{head, first} {
		if err := Sync(context.Background(), quiet(), origin, dir, sha, "main"); err != nil {
			t.Fatalf("Sync(%s): %v", sha[:7], err)
		}
		if got := headOf(t, dir); got != sha {
			t.Errorf("HEAD = %s, want %s", got, sha)
		}
	}
}

// A commit that exists on the origin but not on the project's branch is
// refused before checkout: the working tree keeps the previous deploy.
func TestSyncRefusesCommitOffBranch(t *testing.T) {
	origin, _, head, side := remoteRepo(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := Sync(context.Background(), quiet(), origin, dir, head, "main"); err != nil {
		t.Fatal(err)
	}

	err := Sync(context.Background(), quiet(), origin, dir, side, "main")
	if !errors.Is(err, ErrNotOnBranch) {
		t.Fatalf("want ErrNotOnBranch, got %v", err)
	}
	if got := headOf(t, dir); got != head {
		t.Errorf("refused SHA must not be checked out: HEAD = %s, want %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(dir, "side")); err == nil {
		t.Error("side-branch file leaked into the working tree")
	}

	// No branch (rollback, or allow_any_commit) skips the check.
	if err := Sync(context.Background(), quiet(), origin, dir, side, ""); err != nil {
		t.Fatalf("Sync without branch: %v", err)
	}
	if got := headOf(t, dir); got != side {
		t.Errorf("HEAD = %s, want %s", got, side)
	}
}

// A branch missing from the origin is an error, but not ErrNotOnBranch.
func TestSyncUnknownBranch(t *testing.T) {
	origin, _, head, _ := remoteRepo(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	err := Sync(context.Background(), quiet(), origin, dir, head, "nope")
	if err == nil || errors.Is(err, ErrNotOnBranch) {
		t.Fatalf("want a lookup error for a missing branch, got %v", err)
	}
}

// Only hex object names reach git's argv: no branch names, no options.
func TestSyncRejectsNonHexSHA(t *testing.T) {
	for _, sha := range []string{"", "main", "origin/side", "--upload-pack=touch /tmp/x", "abc"} {
		err := Sync(context.Background(), quiet(), "unused", t.TempDir(), sha, "main")
		if err == nil || !strings.Contains(err.Error(), "invalid commit SHA") {
			t.Errorf("Sync(%q) = %v, want invalid SHA error", sha, err)
		}
	}
}
