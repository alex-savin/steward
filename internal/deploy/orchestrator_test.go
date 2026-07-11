package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
	"github.com/alex-savin/steward/internal/state"
)

// The manifest every harness project shares: a quadlet stack that pins a
// delivered image onto a stable local tag. Health check is omitted, so verify
// is a no-op success — the tests drive failures through the shims instead.
const harnessManifest = `project: demo
stack: quadlet
image:
  local_tag: localhost/demo:live
`

const harnessUnit = `[Container]
ContainerName=demo
Image=localhost/demo:live
[Install]
WantedBy=default.target
`

func noProgress(string) {}

// harness wires an Orchestrator to an isolated HOME (so the quadlet driver
// installs units under a temp dir, not the developer's real ~/.config) and a
// fake `podman`/`systemctl` on PATH that appends every invocation to a log.
// cmdLog() returns that log so a test can assert what was (and wasn't) run.
type harness struct {
	o      *Orchestrator
	dir    string // project working directory (holds .steward.yaml + quadlets/)
	logf   string // path the shims append to
	podman string // body of the podman shim after the shebang; overridable per test
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(dir, ".steward.yaml"), []byte(harnessManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "quadlets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quadlets", "app.container"), []byte(harnessUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		o:      &Orchestrator{log: quiet(), states: state.New(t.TempDir())},
		dir:    dir,
		podman: "echo \"$@\" >> \"$LOGF\"\nexit 0\n",
	}
	return h
}

// installShims writes the podman/systemctl fakes onto PATH. Call after any
// per-test override of h.podman. LOGF is baked into each shim's environment via
// a literal path so the child processes (which don't inherit the test's env
// changes reliably across shells) always find it.
func (h *harness) installShims(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	h.logf = filepath.Join(bin, "cmd.log")
	podman := "#!/bin/sh\nLOGF=" + h.logf + "\n" + h.podman
	systemctl := "#!/bin/sh\nLOGF=" + h.logf + "\necho \"$@\" >> \"$LOGF\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(podman), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(systemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func (h *harness) cmdLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.logf)
	if err != nil {
		return ""
	}
	return string(b)
}

func (h *harness) manifest(t *testing.T) *config.Manifest {
	t.Helper()
	m, err := config.LoadManifest(filepath.Join(h.dir, ".steward.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runGit runs git in dir with a fixed identity, so tests need no global config
// (the harness sets HOME to an empty temp dir).
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

// makeRemote turns h.dir into a git checkout backed by a local bare remote and
// returns the HEAD SHA. Rollback's git.Sync fetches from that remote offline.
func (h *harness) makeRemote(t *testing.T) (sha, cloneURL string) {
	t.Helper()
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	runGit(t, h.dir, "init")
	runGit(t, h.dir, "remote", "add", "origin", remote)
	runGit(t, h.dir, "add", "-A")
	runGit(t, h.dir, "commit", "-m", "initial")
	runGit(t, h.dir, "push", "origin", "HEAD:main")
	return runGit(t, h.dir, "rev-parse", "HEAD"), remote
}

func mustContain(t *testing.T, log, want string) {
	t.Helper()
	if !strings.Contains(log, want) {
		t.Errorf("expected command log to contain %q, got:\n%s", want, log)
	}
}

func mustNotContain(t *testing.T, log, want string) {
	t.Helper()
	if strings.Contains(log, want) {
		t.Errorf("expected command log NOT to contain %q, got:\n%s", want, log)
	}
}

// Fix 1: an image-step failure (transient `podman pull`) happens before deploy
// touches the running stack, so it must be reported as an error WITHOUT the
// destructive rollback (which would stop the healthy stack and restore volumes).
func TestImageStepFailureDoesNotRollback(t *testing.T) {
	h := newHarness(t)
	// podman pull fails; everything else would succeed.
	h.podman = "if [ \"$1\" = pull ]; then echo \"$@\" >> \"$LOGF\"; exit 1; fi\necho \"$@\" >> \"$LOGF\"\nexit 0\n"
	h.installShims(t)

	job := dispatch.Job{
		Project: config.Project{Name: "demo", Path: h.dir, Dir: h.dir, Manifest: ".steward.yaml"},
		Image:   "ghcr.io/o/demo:v1",
		Ref:     "refs/heads/main",
	}
	out := h.o.execute(context.Background(), quiet(), job, noProgress)

	if out.step != "image" || out.err == nil {
		t.Fatalf("want failure at image step, got step=%q err=%v", out.step, out.err)
	}
	if out.rolledBack {
		t.Errorf("image-step failure must NOT trigger rollback")
	}
	log := h.cmdLog(t)
	mustContain(t, log, "pull ghcr.io/o/demo:v1")
	// The stack was never touched: no restart (deploy) and no stop (rollback).
	mustNotContain(t, log, "restart")
	mustNotContain(t, log, "stop")
}

// Fix 3: an image-centric deploy to a GIT project may omit the commit SHA,
// meaning "keep the current checkout, just swap the image". The orchestrator
// must skip git (not run `git checkout ""`) and deploy the existing tree.
func TestImageOnlyGitDeployKeepsCheckout(t *testing.T) {
	h := newHarness(t)
	h.installShims(t)
	// A git project is one with no Path. It needs an existing checkout: a bare
	// .git dir is enough for the "keep current checkout" guard.
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	job := dispatch.Job{
		Project: config.Project{Name: "demo", Repo: "o/demo", Dir: h.dir, Manifest: ".steward.yaml", CloneURL: "https://example.invalid/o/demo.git"},
		SHA:     "", // image-only: no commit pinned
		Image:   "ghcr.io/o/demo:v1",
		Ref:     "refs/heads/main",
	}
	out := h.o.execute(context.Background(), quiet(), job, noProgress)

	if out.err != nil || out.step != "" {
		t.Fatalf("image-only git deploy should succeed, got step=%q err=%v", out.step, out.err)
	}
	log := h.cmdLog(t)
	mustContain(t, log, "pull ghcr.io/o/demo:v1")
	mustContain(t, log, "tag ghcr.io/o/demo:v1 localhost/demo:live")
	mustContain(t, log, "restart")
}

// Fix 2: rolling a GIT+quadlet image-pinned project back to a previous commit
// must also re-point the LocalTag to the previous image. Reverting only the
// .container file leaves the static Image= pointing at the failed image.
func TestGitRollbackRepinsPreviousImage(t *testing.T) {
	h := newHarness(t)
	sha, cloneURL := h.makeRemote(t)
	h.installShims(t)

	job := dispatch.Job{
		Project: config.Project{Name: "demo", Repo: "o/demo", Dir: h.dir, Manifest: ".steward.yaml", CloneURL: cloneURL},
		Ref:     "refs/heads/main",
	}
	prev := state.Project{LastGoodSHA: sha, LastGoodImage: "ghcr.io/o/demo:prev"}

	err := h.o.rollback(context.Background(), quiet(), job, h.manifest(t), nil, nil, prev)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	log := h.cmdLog(t)
	// The whole point of the fix: the previous image is re-pinned to the tag.
	mustContain(t, log, "tag ghcr.io/o/demo:prev localhost/demo:live")
	// And the reverted units are redeployed.
	mustContain(t, log, "restart")
}

// The shared in-place rollback path (local projects, and git image-only)
// re-pins the previous image and redeploys the static definition.
func TestLocalRollbackRepinsPreviousImage(t *testing.T) {
	h := newHarness(t)
	h.installShims(t)

	job := dispatch.Job{
		Project: config.Project{Name: "demo", Path: h.dir, Dir: h.dir, Manifest: ".steward.yaml"},
		Ref:     "refs/heads/main",
	}
	prev := state.Project{LastGoodImage: "ghcr.io/o/demo:prev"}

	err := h.o.rollback(context.Background(), quiet(), job, h.manifest(t), nil, nil, prev)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	log := h.cmdLog(t)
	mustContain(t, log, "tag ghcr.io/o/demo:prev localhost/demo:live")
	mustContain(t, log, "restart")
}
