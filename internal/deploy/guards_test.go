package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
	"github.com/alex-savin/steward/internal/git"
	"github.com/alex-savin/steward/internal/state"
)

// sideCommit pushes a commit to a non-deployed branch of h's origin and
// returns h.dir to main. The commit exists on the origin — fetchable by SHA —
// but is not on the project's branch.
func (h *harness) sideCommit(t *testing.T) string {
	t.Helper()
	runGit(t, h.dir, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(filepath.Join(h.dir, "side.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, h.dir, "add", "-A")
	runGit(t, h.dir, "commit", "-m", "side")
	sha := runGit(t, h.dir, "rev-parse", "HEAD")
	runGit(t, h.dir, "push", "origin", "side")
	runGit(t, h.dir, "checkout", "-q", "main")
	return sha
}

func gitProject(dir, cloneURL string) config.Project {
	return config.Project{Name: "demo", Repo: "o/demo", Branch: "main", Dir: dir, Manifest: ".steward.yaml", CloneURL: cloneURL}
}

// A SHA that is on the origin but not on the project's branch is refused at
// checkout — a pre-deploy error: nothing checked out, no backup, no rollback.
func TestOffBranchCommitRefused(t *testing.T) {
	h := newHarness(t)
	mainSHA, cloneURL := h.makeRemote(t)
	side := h.sideCommit(t)
	h.installShims(t)

	job := dispatch.Job{Project: gitProject(h.dir, cloneURL), SHA: side, Image: "ghcr.io/o/demo:v1", Ref: "refs/heads/main"}
	out := h.o.execute(context.Background(), quiet(), job, noProgress)

	if out.step != "checkout" || !errors.Is(out.err, git.ErrNotOnBranch) {
		t.Fatalf("want ErrNotOnBranch at checkout, got step=%q err=%v", out.step, out.err)
	}
	if out.rolledBack {
		t.Error("a refused commit must not trigger rollback")
	}
	if got := runGit(t, h.dir, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("checkout moved to %s, want it left at %s", got, mainSHA)
	}
	if log := h.cmdLog(t); log != "" {
		t.Errorf("no podman/systemctl call expected, got:\n%s", log)
	}

	// The explicit opt-out deploys it.
	job.Project.AllowAnyCommit = true
	if out := h.o.execute(context.Background(), quiet(), job, noProgress); out.err != nil {
		t.Fatalf("allow_any_commit deploy: step=%q err=%v", out.step, out.err)
	}
}

// The branch head deploys normally with the check on.
func TestOnBranchCommitDeploys(t *testing.T) {
	h := newHarness(t)
	sha, cloneURL := h.makeRemote(t)
	h.installShims(t)

	job := dispatch.Job{Project: gitProject(h.dir, cloneURL), SHA: sha, Ref: "refs/heads/main"}
	if out := h.o.execute(context.Background(), quiet(), job, noProgress); out.err != nil {
		t.Fatalf("on-branch deploy: step=%q err=%v", out.step, out.err)
	}
}

// Rollback is not subject to the branch check: a last-good SHA that a
// force-push has since removed from the branch must still be restorable.
func TestRollbackToCommitNoLongerOnBranch(t *testing.T) {
	h := newHarness(t)
	_, cloneURL := h.makeRemote(t)
	side := h.sideCommit(t) // stands in for a commit force-pushed off main
	h.installShims(t)

	job := dispatch.Job{Project: gitProject(h.dir, cloneURL), Ref: "refs/heads/main"}
	prev := state.Project{LastGoodSHA: side, LastGoodImage: "ghcr.io/o/demo:prev"}
	if err := h.o.rollback(context.Background(), quiet(), job, h.manifest(t), nil, nil, prev); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := runGit(t, h.dir, "rev-parse", "HEAD"); got != side {
		t.Errorf("rollback checked out %s, want %s", got, side)
	}
}

// An image outside the project's allowlist is refused before secrets, backup
// or pull — reported as an "image" step error, no rollback.
func TestImageOutsideAllowlistRefused(t *testing.T) {
	h := newHarness(t)
	h.installShims(t)

	job := dispatch.Job{
		Project: config.Project{Name: "demo", Path: h.dir, Dir: h.dir, Manifest: ".steward.yaml",
			Images: []string{"registry.savin.nyc/familytree/app"}},
		Image: "registry.savin.nyc/familytree/app-evil:v1",
	}
	out := h.o.execute(context.Background(), quiet(), job, noProgress)

	if out.step != "image" || out.err == nil || !strings.Contains(out.err.Error(), "allowlist") {
		t.Fatalf("want allowlist refusal at image step, got step=%q err=%v", out.step, out.err)
	}
	if out.rolledBack {
		t.Error("a refused image must not trigger rollback")
	}
	if out.manifest == nil {
		t.Error("manifest should be attached so the refusal is notified")
	}
	if log := h.cmdLog(t); log != "" {
		t.Errorf("no podman/systemctl call expected, got:\n%s", log)
	}
}

// A digest reference (repo@sha256:…) passes the allowlist, is pulled and
// pinned as-is, is recorded as last_good_image, and rollback re-pins it.
func TestDigestImageDeployAndRollback(t *testing.T) {
	h := newHarness(t)
	h.installShims(t)

	digest := "registry.savin.nyc/familytree/app@sha256:" + strings.Repeat("ab", 32)
	job := dispatch.Job{
		Project: config.Project{Name: "demo", Path: h.dir, Dir: h.dir, Manifest: ".steward.yaml",
			Images: []string{"registry.savin.nyc/familytree/app"}},
		Image: digest,
	}
	if out := h.o.execute(context.Background(), quiet(), job, noProgress); out.err != nil {
		t.Fatalf("digest deploy: step=%q err=%v", out.step, out.err)
	}
	log := h.cmdLog(t)
	mustContain(t, log, "pull "+digest)
	mustContain(t, log, "tag "+digest+" localhost/demo:live")

	prev, err := h.o.states.Load("demo")
	if err != nil {
		t.Fatal(err)
	}
	if prev.LastGoodImage != digest {
		t.Fatalf("last_good_image = %q, want %q", prev.LastGoodImage, digest)
	}

	// A later deploy fails; rollback re-pins the recorded digest.
	if err := os.Truncate(h.logf, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.o.rollback(context.Background(), quiet(), job, h.manifest(t), nil, nil, prev); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	mustContain(t, h.cmdLog(t), "tag "+digest+" localhost/demo:live")
}
