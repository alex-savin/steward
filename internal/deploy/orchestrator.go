package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
	"github.com/alex-savin/steward/internal/git"
	"github.com/alex-savin/steward/internal/notify"
	"github.com/alex-savin/steward/internal/state"
)

// checkoutTimeout bounds clone/fetch/checkout, which run before the manifest
// (and its deployment.timeout) is available — a hung fetch must not wedge the
// project's worker goroutine forever.
const checkoutTimeout = 10 * time.Minute

// rollbackGrace bounds rollback with a fresh deadline: the most common reason
// we roll back at all is that the deployment timeout already expired.
const rollbackGrace = 5 * time.Minute

// notifyGrace bounds the final notification fan-out.
const notifyGrace = 30 * time.Second

// logCaptureGrace bounds the post-failure log grab, and logTailLines caps how
// much of the stack's output we keep.
const (
	logCaptureGrace = 30 * time.Second
	logTailLines    = 50
)

// Orchestrator executes the full deployment lifecycle for one job. It is the
// background worker behind the webhook's 202 Accepted.
type Orchestrator struct {
	cfg      *config.Config
	log      *slog.Logger
	github   *notify.GitHubClient   // nil when no token configured; methods are nil-safe
	telegram *notify.TelegramClient // nil when no bot token; methods are nil-safe
	states   *state.Store
}

// New builds the orchestrator. The Telegram token's format was already checked
// at config load, so an error here means telego rejected something we accepted;
// notifications are disabled rather than downing the agent over a push channel.
func New(cfg *config.Config, log *slog.Logger) *Orchestrator {
	telegram, err := notify.NewTelegram(cfg.TelegramToken(), cfg.Telegram.APIServer)
	if err != nil {
		log.Error("telegram notifications disabled", "error", err)
	}
	return &Orchestrator{
		cfg:      cfg,
		log:      log,
		github:   notify.NewGitHub(cfg.GitHubToken(), cfg.GitHub.APIURL),
		telegram: telegram,
		states:   state.New(cfg.StateDir()),
	}
}

// outcome collects what happened, for notifications and dispatcher feedback.
type outcome struct {
	manifest    *config.Manifest
	step        string // lifecycle step that failed ("" on success)
	err         error
	rolledBack  bool
	rollbackErr error
	// logs is the tail of the stack's own output, captured after a deploy or
	// verify failure and before rollback wipes the evidence.
	logs string
}

// Run is the dispatch.RunFunc entry point. The webhook handler has already
// returned 202 by the time this executes on a dispatcher goroutine.
func (o *Orchestrator) Run(ctx context.Context, job dispatch.Job, progress dispatch.Progress) dispatch.Result {
	log := o.log.With("project", job.Project.Name, "job", job.ID, "sha", shortSHA(job.SHA))
	started := time.Now()
	log.Info("deployment starting", "ref", job.Ref, "pusher", job.Pusher, "message", job.Message)

	o.setCommitStatus(ctx, job, notify.StatePending, "deployment in progress")

	out := o.executeSafe(ctx, log, job, progress)

	// Notifications must go out even when the deployment context is already
	// cancelled or expired — that failure is exactly what we need to report.
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyGrace)
	defer cancel()
	o.report(notifyCtx, log, job, out, time.Since(started).Round(time.Second))

	res := dispatch.Result{}
	if out.manifest != nil {
		res.Mode = out.manifest.Deployment.Mode
	}
	return res
}

// executeSafe converts a panic anywhere in the pipeline into a failed
// outcome, so the final commit status and notifications still go out instead
// of leaving the SHA stuck at "pending". (The dispatcher has its own recover
// as a last-resort backstop for the worker goroutine.)
func (o *Orchestrator) executeSafe(parent context.Context, log *slog.Logger, job dispatch.Job, progress dispatch.Progress) (out outcome) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("deployment panicked", "panic", r)
			out = outcome{step: "internal", err: fmt.Errorf("internal panic: %v", r)}
		}
	}()
	return o.execute(parent, log, job, progress)
}

// step reports the current lifecycle step to the dispatcher (for GET /status).
// A nil Progress is fine — tests construct the orchestrator without one.
func step(progress dispatch.Progress, name string) {
	if progress != nil {
		progress(name)
	}
}

// execute walks the strict lifecycle sequence, stopping at the first failure:
//
//	checkout -> manifest -> [1] decrypt -> [2] backup -> [3] deploy
//	          -> [4] verify -> [5] rollback (only if 3/4 failed)
func (o *Orchestrator) execute(parent context.Context, log *slog.Logger, job dispatch.Job, progress dispatch.Progress) outcome {
	// Checkout: bring the working copy to the exact pushed commit, under its
	// own deadline (the manifest's timeout isn't known until after this).
	step(progress, "checkout")
	workdir := job.Project.Dir
	syncCtx, cancelSync := context.WithTimeout(parent, checkoutTimeout)
	err := git.Sync(syncCtx, log, job.Project.CloneURL, workdir, job.SHA)
	cancelSync()
	if err != nil {
		return outcome{step: "checkout", err: err}
	}

	// Manifest: the repo's committed .steward.yaml drives everything below.
	step(progress, "manifest")
	m, err := config.LoadManifest(filepath.Join(workdir, job.Project.Manifest))
	if err != nil {
		return outcome{step: "manifest", err: err}
	}
	log = log.With("stack", m.Stack, "environment", m.Environment)

	// One deadline (deployment.timeout) covers the whole lifecycle.
	ctx, cancel := context.WithTimeout(parent, m.Deployment.Timeout.Std())
	defer cancel()

	// Deployment identity, exported to every child process (see jobEnv).
	env := jobEnv(m, job.Project.Name, job.Ref, job.SHA)

	stack, err := NewStack(m, workdir, env, log)
	if err != nil {
		return outcome{manifest: m, step: "stack", err: err}
	}

	prev, err := o.states.Load(job.Project.Name)
	if err != nil {
		log.Warn("could not load previous state; rollback will be volume-only", "error", err)
	}

	var snap *Snapshot

	// The strict sequence. Order matters: secrets before backup (backup
	// commands may need the decrypted env), backup before deploy (so rollback
	// has something to restore), verify only after deploy.
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"secrets", func(ctx context.Context) error { // Step 1 — SOPS/age decryption
			return decryptSecrets(ctx, log, workdir, m)
		}},
		{"backup", func(ctx context.Context) error { // Step 2 — hooks, snapshots, rotation
			var err error
			snap, err = runBackup(ctx, log, workdir, m, job.SHA, env)
			return err
		}},
		{"deploy", func(ctx context.Context) error { // Step 3 — compose up / quadlet install+restart
			return stack.Deploy(ctx)
		}},
		{"verify", func(ctx context.Context) error { // Step 4 — health check with retries
			return verifyHealth(ctx, log, workdir, m.HealthCheck, env)
		}},
	}

	for _, s := range steps {
		log.Info("lifecycle step", "step", s.name)
		step(progress, s.name)
		if err := s.run(ctx); err != nil {
			log.Error("lifecycle step failed", "step", s.name, "error", err)
			out := outcome{manifest: m, step: s.name, err: err}

			// Step 5 — rollback, but only once the stack may have been
			// touched. Failures in secrets/backup abort before any change.
			if s.name == "deploy" || s.name == "verify" {
				// Capture the stack's own output first: rollback restarts the
				// previous version and destroys the only evidence of why this
				// one didn't come up.
				out.logs = captureLogs(parent, log, stack)

				step(progress, "rollback")
				out.rolledBack = true
				out.rollbackErr = o.rollback(parent, log, job, m, stack, snap, prev.LastGoodSHA)
			}
			return out
		}
	}

	// Success: this commit becomes the rollback target for next time.
	if err := o.states.Save(job.Project.Name, state.Project{LastGoodSHA: job.SHA, DeployedAt: time.Now().UTC()}); err != nil {
		log.Warn("could not persist last-good state", "error", err)
	}
	log.Info("deployment succeeded")
	return outcome{manifest: m}
}

// captureLogs grabs the tail of the failed stack's output. It gets a fresh,
// short deadline derived from parent: the deployment context is usually already
// expired by the time we want the logs, and a failed notification is better
// than a hung deployment. Never returns an error — this is diagnostics.
func captureLogs(parent context.Context, log *slog.Logger, stack Stack) string {
	if stack == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), logCaptureGrace)
	defer cancel()

	out, err := stack.Logs(ctx, logTailLines)
	if err != nil && out == "" {
		log.Warn("could not capture stack logs", "error", err)
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	log.Error("stack logs at failure", "tail", out)
	return out
}

// rollback reverts to the last known-good deployment: stop the failed stack,
// restore the volume snapshots taken in Step 2, check the previous commit
// back out, re-decrypt its secrets, and redeploy its stack definition.
//
// It runs under a fresh deadline derived from parent (the dispatcher's base
// context): parent has no deployment timeout — the expired deployment context
// was a child created in execute — but it IS cancelled on forced shutdown,
// which must abort rollback too.
func (o *Orchestrator) rollback(parent context.Context, log *slog.Logger, job dispatch.Job, m *config.Manifest, failed Stack, snap *Snapshot, lastGoodSHA string) error {
	if lastGoodSHA == "" && (snap == nil || len(snap.Volumes) == 0) {
		return errors.New("nothing to roll back to (no previous successful deploy recorded)")
	}
	ctx, cancel := context.WithTimeout(parent, rollbackGrace)
	defer cancel()

	log.Warn("rolling back", "to", shortSHA(lastGoodSHA))
	var errs []error

	// Quiesce the failed stack before touching its data (best effort).
	if failed != nil && snap != nil && len(snap.Volumes) > 0 {
		if err := failed.Stop(ctx); err != nil {
			log.Warn("could not stop failed stack before restore", "error", err)
		}
	}

	if err := restoreVolumes(ctx, log, snap); err != nil {
		errs = append(errs, err)
	}

	if lastGoodSHA != "" {
		if err := git.Sync(ctx, log, job.Project.CloneURL, job.Project.Dir, lastGoodSHA); err != nil {
			errs = append(errs, fmt.Errorf("checkout last-good %s: %w", shortSHA(lastGoodSHA), err))
		} else {
			// The previous commit may declare a different stack layout or
			// secrets set — honor ITS manifest, not the broken one.
			prevManifest, err := config.LoadManifest(filepath.Join(job.Project.Dir, job.Project.Manifest))
			if err != nil {
				log.Warn("previous manifest unreadable; reusing current", "error", err)
				prevManifest = m
			}
			if err := decryptSecrets(ctx, log, job.Project.Dir, prevManifest); err != nil {
				errs = append(errs, err)
			}
			// The restored commit's env: its compose file must resolve to the
			// image tag it was originally deployed with, not the failed SHA.
			prevEnv := jobEnv(prevManifest, job.Project.Name, job.Ref, lastGoodSHA)
			prevStack, err := NewStack(prevManifest, job.Project.Dir, prevEnv, log)
			switch {
			case err != nil:
				errs = append(errs, err)
			default:
				if err := prevStack.Deploy(ctx); err != nil {
					errs = append(errs, fmt.Errorf("redeploy previous version: %w", err))
					break
				}
				// `compose up -d` and `systemctl restart` return when the
				// container STARTS, not when the app is healthy. Without this
				// check the agent would report "rolled back" while the restored
				// stack crash-loops — for instance when the restored volume
				// leaves the database in a state the old code rejects.
				// Use the previous commit's health check, not the broken one's.
				if err := verifyHealth(ctx, log, job.Project.Dir, prevManifest.HealthCheck, prevEnv); err != nil {
					errs = append(errs, fmt.Errorf("rolled-back version did not become healthy: %w", err))
				}
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		log.Error("rollback finished with errors", "error", err)
		return err
	}
	log.Info("rollback complete", "restored", shortSHA(lastGoodSHA))
	return nil
}

// report delivers the final feedback: GitHub commit status + ntfy push.
func (o *Orchestrator) report(ctx context.Context, log *slog.Logger, job dispatch.Job, out outcome, elapsed time.Duration) {
	var ghState, desc string
	switch {
	case out.err == nil:
		ghState = notify.StateSuccess
		desc = fmt.Sprintf("deployed in %s", elapsed)
	case out.step == "deploy" || out.step == "verify":
		ghState = notify.StateFailure
		desc = fmt.Sprintf("%s failed: %v", out.step, out.err)
	default:
		// Infrastructure trouble before the stack was touched.
		ghState = notify.StateError
		desc = fmt.Sprintf("%s failed: %v", out.step, out.err)
	}
	if out.rolledBack {
		if out.rollbackErr == nil {
			desc += " (rolled back)"
		} else {
			desc += " (ROLLBACK FAILED)"
		}
	}
	o.setCommitStatus(ctx, job, ghState, desc)

	// The manifest is unreadable when checkout or parsing failed; there is
	// nowhere to route a push notification, so the commit status is all we have.
	if out.manifest == nil {
		return
	}

	name := out.manifest.Project
	if out.manifest.Environment != "" {
		name += " (" + out.manifest.Environment + ")"
	}

	// One rendering of the result, shared by every push channel. job.Message is
	// attacker-influenced (it is a commit subject); channels escape it.
	var title string
	var lines []string
	if out.err == nil {
		title = "Deployed " + name
		lines = []string{fmt.Sprintf("%s in %s", job.Project.Repo, elapsed)}
	} else {
		title = "Deploy FAILED: " + name
		lines = []string{
			job.Project.Repo,
			fmt.Sprintf("step %q: %v", out.step, out.err),
		}
		if out.rolledBack {
			if out.rollbackErr == nil {
				lines = append(lines, "Rolled back to previous version.")
			} else {
				lines = append(lines, fmt.Sprintf("ROLLBACK FAILED: %v", out.rollbackErr))
			}
		}
	}
	if job.Message != "" {
		lines = append(lines, "“"+job.Message+"”")
	}

	if n := out.manifest.Notifications.Ntfy; n != nil {
		// ntfy is plain text with no link support, so the SHA goes in the body.
		// (Telegram renders it as a link instead — see TelegramMessage.SHA.)
		body := append([]string{shortSHA(job.SHA)}, lines...)
		if out.logs != "" {
			body = append(body, "", "--- logs ---", lastLines(out.logs, notifyLogLines))
		}
		msg := notify.NtfyMessage{Title: title, Body: strings.Join(body, "\n"), Priority: "default", Tags: "rocket"}
		if out.err != nil {
			msg.Priority = "high"
			msg.Tags = "rotating_light"
		}
		if err := notify.SendNtfy(ctx, n, msg); err != nil {
			log.Warn("ntfy notification failed", "error", err)
		}
	}

	if t := out.manifest.Notifications.Telegram; t != nil {
		if o.telegram == nil {
			log.Warn("manifest configures notifications.telegram but the agent has no bot token; skipping")
		} else if err := o.telegram.Send(ctx, t, notify.TelegramMessage{
			Title:     title,
			Lines:     lines,
			Failed:    out.err != nil,
			SHA:       shortSHA(job.SHA),
			CommitURL: o.commitURL(job),
			Log:       lastLines(out.logs, notifyLogLines),
		}); err != nil {
			log.Warn("telegram notification failed", "error", err)
		}
	}
}

// notifyLogLines is how much of the captured log tail reaches a notification.
// The full tail is in the agent's own log; a push message just needs the crash.
const notifyLogLines = 12

// lastLines returns the final n lines of s.
func lastLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// commitURL links the SHA in a notification back to GitHub. Returns "" for a
// GitHub Enterprise install, whose web host we cannot infer from the API URL.
func (o *Orchestrator) commitURL(job dispatch.Job) string {
	if o.cfg.GitHub.APIURL != "https://api.github.com" {
		return ""
	}
	return "https://github.com/" + job.Project.Repo + "/commit/" + job.SHA
}

// ReportSuperseded marks a queued commit that a newer push replaced before it
// ever deployed. Without it the SHA keeps whatever status it had and a CI job
// blocking on that commit status waits until it times out. Deliberately posts
// "error", not "success": that commit was never deployed, and saying otherwise
// would be a lie. No ntfy/Telegram — superseding a queued push is routine.
func (o *Orchestrator) ReportSuperseded(dropped, by dispatch.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyGrace)
	defer cancel()

	o.log.Info("reporting superseded deployment",
		"project", dropped.Project.Name, "sha", shortSHA(dropped.SHA), "superseded_by", shortSHA(by.SHA))
	o.setCommitStatus(ctx, dropped, notify.StateError,
		fmt.Sprintf("superseded by %s; not deployed", shortSHA(by.SHA)))
}

// setCommitStatus posts to the GitHub Commit Statuses API. The status context
// is stable per project so pending/success/failure collapse into one check.
func (o *Orchestrator) setCommitStatus(ctx context.Context, job dispatch.Job, ghState, desc string) {
	statusCtx := "steward/" + job.Project.Name
	if err := o.github.SetCommitStatus(ctx, job.Project.Repo, job.SHA, ghState, statusCtx, desc); err != nil {
		o.log.Warn("github commit status failed", "project", job.Project.Name, "state", ghState, "error", err)
	}
}
