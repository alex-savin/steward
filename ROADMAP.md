# Roadmap

Work that is known, scoped, and deliberately not done yet. Items leave this file
when they ship or when we decide against them.

## Podman secrets as a secrets sink

**Status:** planned · **Size:** ~half a day · **Blocks:** nothing

Today `secrets.provider: sops` has exactly one sink: decrypt to a `0600` file
inside the checkout ([`internal/deploy/secrets.go`](internal/deploy/secrets.go)).
Quadlet units consume it with `EnvironmentFile=`, compose with file-based
`secrets:`. That works, but it puts plaintext on disk and it cannot satisfy a
unit that uses Podman's own `Secret=` key.

Quadlet's `Secret=` maps to `podman run --secret` and requires the secret to
**already exist in Podman's secret store**. There is no `.secret` quadlet unit
type — the types are Container, Pod, Kube, Network, Volume, Build, Image, and
Artifact — so nothing committed to a repo can create one. A repo using `Secret=`
today fails at `systemctl --user restart` with a secret-not-found error, and the
agent rolls the deployment back.

### Proposed shape

A second sink on the existing `secrets` block, so SOPS stays the single source of
truth:

```yaml
secrets:
  provider: sops
  key_file: /opt/steward/keys/age.txt
  files:
    - source: secrets/production.enc.env
      destination: .env                  # existing: file in the checkout
  podman:                                # new: Podman secret store
    - source: secrets/db_password.enc.txt
      name: myapp_db_password
```

Implementation: pipe `sops --decrypt` stdout straight into
`podman secret create --replace <name> -`. Verified against the Podman 6.0.1 man
pages — `podman secret create [options] name file|-` accepts `-` for stdin, and
`--replace` exists. **The plaintext never touches disk**, which is strictly better
than the current `.env` at `0600`.

Rollback needs no new code: [`Orchestrator.rollback`](internal/deploy/orchestrator.go)
already re-runs `decryptSecrets` against the *previous commit's* manifest, so the
previous ciphertext is re-decrypted and re-created.

### Open design question

Podman secrets are **host state, not repo state**. Unlike quadlet units — which we
converge using an ownership file — a secret cannot be read back to diff it, so
every declared secret gets `--replace`d on every deploy.

The question is what to do with a secret that *disappears* from the manifest. The
current lean is **do not delete it**: names are global to the Podman store, and an
unrelated container may share one. That trades a possible orphan for never
breaking a neighbour. Revisit if it causes confusion in practice.

### Out of scope

Compose stacks keep using file-based secrets. `docker compose` has no equivalent
of the Podman secret store, and a `podman-compose` special case is not worth the
divergence.

---

## Shipped

Kept for the record; the reasoning is in git history and the docs.

- **Tests for the irreversible paths** — `restoreVolumes` (rsync `--delete` over a
  live directory), `rotateBackups` / `snapshotDirPattern`, `validSignature`, plus
  the dispatcher's queue/reject state machine under `-race`. Every one was
  mutation-tested: loosening the rotation regex, dropping `--delete`, dropping the
  trailing slash, or accepting a missing HMAC header all turn the suite red.
  `internal/deploy`, `internal/dispatch`, `internal/server`, `internal/state` and
  `pkg/lockfile` now have tests.
- **Rollback verifies what it restored.** Rollback re-runs the *previous* commit's
  health check after redeploying it. A rollback that deploys but never becomes
  healthy is reported as `ROLLBACK FAILED`, never as "rolled back".
- **Queue mode coalesces to the newest SHA.** A burst of pushes now deploys once.
  Superseded commits get a GitHub commit status of `error`
  ("superseded by <sha>; not deployed") so a CI job blocking on that commit does
  not hang. `defaults.max_queue` is deprecated and ignored.
- **Container logs on a failed deploy.** The tail of `compose logs` /
  `journalctl --user` is captured *before* rollback destroys it, logged, and
  included in the ntfy and Telegram notifications.
- **`steward validate [path…]`** lints manifests offline with the same
  parser the agent uses, so a typo fails in CI instead of on the deploy host.
- **`/status` reports the running deployment's lifecycle step** and when it
  entered it, distinguishing a slow deploy from a wedged one.

## Also queued

**`.artifact` quadlet unit type.** `quadletExtensions`
([`internal/deploy/quadlet.go:20`](internal/deploy/quadlet.go#L20)) omits
`.artifact`, which Podman has supported (experimentally) since 5.x. An
`app.artifact` in a repo's quadlet dir is silently skipped: not installed, not
tracked in the ownership file, not cleaned up on rollback. One-word fix; worth a
test alongside it. Artifact units generate a oneshot service, so `serviceFor`
should keep returning `""` for them — systemd activates them as a dependency.

**Independent security review.** The adversarial review of the initial
implementation covered concurrency, correctness, and requirements, but the
security-dimension agent hit a session limit and never reported. HMAC validation,
manifest path containment, and secret file permissions were designed and
hand-checked, but they have not had a dedicated hostile pass. (A later security
pass over the Telegram channel did land, and found a real bot-token leak through
`*url.Error` — evidence the outstanding pass is worth running.)

**Drain queued deployments on shutdown.** `Dispatcher.Shutdown`
([`internal/dispatch/dispatcher.go`](internal/dispatch/dispatcher.go)) finishes the
running deployment but drops jobs already acknowledged with `202 queued`, logging
what it dropped. That is a deliberate trade — a queued job may target a commit the
operator is mid-rollback on — but draining them within `shutdown_grace` would be
less surprising, and the `202` arguably promised it.

---

## Considered and declined

**`repository_dispatch` as a second trigger.** It would need a second inbound path
with its own auth. Reusing the push-event shape means GitHub's webhook and a CI
dispatch are byte-identical to the agent: one authenticated entry point, one
validation path, one set of tests. See
[docs/github-actions.md](docs/github-actions.md#pattern-b--ci-gated-deploy-recommended).

**A rollback subcommand.** A rollback *is* a deployment of an earlier commit.
Dispatching the old SHA exercises the same code path as any other deploy, rather
than a second, rarely-run one. See
[docs/operations.md](docs/operations.md#manual-operations).

**Tag and release deploys** (`refs/tags/v*`). Real gap — a tag push is answered
`200 ignored`. But the supported paths already cover it: a CI job triggered
`on: push: tags:` dispatches the tag's SHA through the existing webhook
([Pattern B](docs/github-actions.md#pattern-b--ci-gated-deploy-recommended)), and
`git.Sync` checks out any SHA. Without CI, a `production` branch works. Adding
ref-type and glob matching would fork the single, exact repo+branch matching path
for a narrow persona.

**Monorepo path filters** (`paths: [grafana/**]`). GitHub Actions does this in one
line of workflow YAML, *before* tests and the image build even run. The agent
would only see changed paths for the raw-webhook pattern, whose apps are the ones
least hurt by a redundant redeploy. The tool's grain is one repo per deployable
unit.

**A scheduled trigger** for re-pulling moving tags. `podman-auto-update.timer`
does this better (it compares digests and only restarts on a real change), and a
`cron` line calling `scripts/steward-dispatch.sh` covers the compose case with
zero new code. An internal timer would be a second, rarely-exercised path into
`Enqueue` — the same objection that killed `repository_dispatch`.

**A deploy-history store.** Failed attempts aren't persisted; `state/<project>.json`
holds only the last-good SHA. But GitHub commit statuses already are the history,
and `journalctl` holds the detail. A local event log would duplicate both.

**Free-space-based backup GC.** Retention is count-only (`keep_last`). Pruning by
disk usage sounds prudent, but a deploy agent silently deleting more backups than
it was told to is a worse failure than a full disk, which fails loudly at the
`cp -a` and aborts *before* touching the running stack.

One thing declined but worth remembering: `git.Sync` never runs `git clean`, so a
plaintext secret decrypted by an old commit lingers (mode `0600`) after that entry
is removed from `secrets.files`. Judged acceptable under the trust model — anyone
who can push already runs commands as this user — but revisit if secrets ever
move to a less-privileged location.
