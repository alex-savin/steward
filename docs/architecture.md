# Architecture

How a push becomes a running container, and what happens when it doesn't.

- [Components](#components)
- [Request path](#request-path)
- [Concurrency model](#concurrency-model)
- [The deployment lifecycle](#the-deployment-lifecycle)
- [Contexts and timeouts](#contexts-and-timeouts)
- [Failure matrix](#failure-matrix)
- [Persistent state](#persistent-state)
- [Environment exported to child processes](#environment-exported-to-child-processes)

## Components

| Package | Responsibility |
| --- | --- |
| [cmd/steward](../cmd/steward/main.go) | Flags, the offline `validate` subcommand, config load, single-instance lock, graceful shutdown |
| [internal/server](../internal/server/) | HTTP listener, HMAC webhook validation, `/healthz`, `/status` |
| [internal/dispatch](../internal/dispatch/dispatcher.go) | Per-project serialization (queue/reject), background workers |
| [internal/deploy](../internal/deploy/) | The lifecycle engine and the two stack drivers |
| [internal/git](../internal/git/repo.go) | Working checkout: clone, fetch, detached checkout of an exact SHA |
| [internal/config](../internal/config/) | Agent config + `.steward.yaml` manifest schema and validation |
| [internal/state](../internal/state/state.go) | Last known-good commit per project — the rollback target |
| [internal/notify](../internal/notify/) | GitHub Commit Statuses API, ntfy, Telegram |
| [internal/execx](../internal/execx/execx.go) | Uniform subprocess execution: context kill, output capture and capping |
| [pkg/lockfile](../pkg/lockfile/lockfile.go) | `flock(2)` single-instance lock |

The agent is a single static binary with no database. Everything it remembers
between restarts lives in two places on disk: the git checkouts, and one small
JSON file per project under `<data_dir>/state/`.

## Request path

```
GitHub (or GitHub Actions)
   │  POST /webhook/github   X-Hub-Signature-256: sha256=…
   ▼
Nginx  (TLS termination, sets X-Forwarded-For)
   │
   ▼
server.handleWebhook
   │  1. read body (capped at 25 MB)
   │  2. HMAC-SHA256 compare, constant time  ──── mismatch ──▶ 401
   │  3. X-GitHub-Event: ping → 200 pong; anything but push → 200 ignored
   │  4. parse push payload; branch deletion → 200 ignored
   │  5. match repository.full_name + branch to a configured project
   │                                          ──── no match ──▶ 200 ignored
   │  6. dispatcher.Enqueue(job)
   ▼
   ├── started / queued ──▶ 202 Accepted  {"status":"started","job_id":"…"}
   ├── rejected          ──▶ 409 Conflict
   └── shutting down     ──▶ 503 Service Unavailable
                              │
                              ▼  (background goroutine, HTTP request already answered)
                        deploy.Orchestrator.Run
```

The handler never blocks on a deployment. Validation is a signature check and a
JSON parse; everything after `Enqueue` happens on a worker goroutine the
dispatcher owns.

**Authentication is the HMAC signature and nothing else.** `X-Forwarded-For` is
parsed only to label log lines. With Nginx's standard
`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for`, Nginx *appends*
the peer address it actually saw, so the agent reads the **rightmost** entry —
the leftmost is whatever the client claimed.

## Concurrency model

One worker goroutine per project, created on that project's first job and
retired when its queue drains. Two deployments of the same project can never
overlap; two *different* projects deploy concurrently.

When a job arrives while that project is already deploying, the project's
concurrency mode decides:

| `deployment.mode` | Behavior | HTTP |
| --- | --- | --- |
| `queue` (default) | Appended to the project's queue, run in FIFO order | `202` `{"status":"queued"}` |
| `reject` | Refused; the running deployment is left alone | `409` |

**Queue mode coalesces.** A project's queue holds at most one pending job: a new
push replaces whatever was waiting, because only the newest commit is worth
deploying. Superseded commits get a GitHub commit status of `error`
("superseded by <sha>; not deployed") so a CI job blocking on that commit
doesn't wait forever. `defaults.max_queue` is therefore obsolete — it is still
accepted, and ignored.

**Where the mode comes from.** `deployment.mode` lives in the repo's
`.steward.yaml`, which the agent can only read after checking the repo out. So
on startup the dispatcher seeds each project's mode by reading the manifest from
the project's *existing* checkout ([`Options.ResolveMode`](../internal/dispatch/dispatcher.go)),
and updates it after every deployment. Two consequences worth knowing:

- The **very first deployment of a project** (no checkout on disk yet) uses
  `defaults.deploy_mode` from the agent config. If you rely on `reject`, set
  `defaults.deploy_mode: reject` too.
- After a restart the mode is recovered from disk, so `reject` is not silently
  downgraded to `queue`.

A panic inside a deployment is recovered twice: once in the orchestrator (so the
commit status and notifications still go out, reported as an `error`), and once
in the dispatcher as a backstop, so a panicking job can never strand the
project's queue.

## The deployment lifecycle

```
checkout ──▶ manifest ──▶ [1] secrets ──▶ [2] backup ──▶ [3] deploy ──▶ [4] verify ──▶ success
                                │              │              │              │
                                ▼              ▼              ▼              ▼
                              abort          abort      [5] rollback   [5] rollback
                                │              │              │              │
                                └──────────────┴──────────────┴──────────────┘
                                                     ▼
                                              notify (always)
```

**checkout** — `git clone` on first use, then `git fetch --force --tags` and
`git checkout --force --detach <sha>`. Deploying by SHA (not by branch) means
the deployed tree is exactly what the webhook announced, and any previously
deployed commit is a valid rollback target. If the SHA isn't reachable from a
branch head (force-push race), the agent fetches the commit directly and retries.

**manifest** — `.steward.yaml` is read from the checkout. Unknown keys are a
hard error, so a typo disables nothing silently.

**[1] secrets** — for each `secrets.files` entry, `sops --decrypt <source>` runs
with `SOPS_AGE_KEY_FILE` pointing at `secrets.key_file`; stdout is written to
`destination` with mode `0600`. Plaintext never passes through a log line or an
error message: stdout and stderr are captured separately.

**[2] backup** — `backup.commands` run first (in the checkout dir, via
`/bin/sh -c`), then each `backup.volumes` entry is copied with `cp -a` into
`<destination>/<timestamp>-<short-sha>/`, then rotation deletes all but the
newest `retention.keep_last` snapshots. Commands run **before** the copies so a
dump written into a backed-up directory lands in *this* run's snapshot.

Rotation only ever removes directories matching `^\d{8}T\d{6}Z-[0-9a-f]{1,12}$`.
Anything else living in the destination is untouched.

**[3] deploy** — see [the stack drivers](#stack-drivers) below.

**[4] verify** — `health_check` runs up to `retries` times, each attempt bounded
by `timeout`, with linear backoff (2s, 4s, 6s…) between attempts. `http` passes
on any 2xx; `command` passes on exit 0. No `health_check` block means the deploy
is considered successful once step 3 returns.

**[5] rollback** — only reachable from steps 3 and 4, because only those touch
the running system. Failures in checkout, manifest, secrets, or backup abort
with nothing to undo. Before rolling back, the agent captures the tail of the
failed stack's own output (`compose logs` / `journalctl --user`) — rollback is
about to restart the previous version and destroy the only evidence of why this
one didn't come up. Rollback then:

1. stops the failed stack (so containers aren't writing into data being restored),
2. restores each volume snapshot taken in step 2 (`rsync -a --delete`, or `cp -a`
   when rsync is absent; single files are copied directly),
3. checks out the last known-good SHA,
4. loads **that commit's** manifest — not the broken one — and re-decrypts its
   secrets,
5. redeploys its stack,
6. **re-runs that commit's health check.** `compose up -d` and `systemctl restart`
   return when a container *starts*, not when the app is healthy. Without this the
   agent would report "rolled back" while the restored stack crash-loops — say,
   because the restored volume left the database in a state the old code rejects.
   A rollback that deploys but never becomes healthy is reported as
   `ROLLBACK FAILED`, not as a success.

If no previous successful deployment was ever recorded and no snapshots exist,
rollback reports `nothing to roll back to` rather than guessing.

**notify** — always runs, under a context detached from the deployment's (an
expired deadline is precisely the thing that must be reported). Posts a commit
status and, if configured, a push to ntfy and/or Telegram. On a deploy or verify
failure the push carries the tail of the stack logs captured above, so the
notification says *why* the app didn't come up rather than only that it didn't.

### Stack drivers

**`docker-compose`** runs `docker compose -f <file> up -d --remove-orphans` in
the checkout directory. Dependencies marked `start_first: true` are brought up
individually beforehand (`up -d <name>`). The compose implementation is
autodetected: `docker compose`, then `docker-compose`, then `podman-compose`.
`Stop` is `compose stop` — networks and volumes survive.

**`quadlet`** copies `*.container`, `*.pod`, `*.volume`, `*.network`, `*.kube`,
`*.image` and `*.build` files from the repo's `quadlet.dir` into
`~/.config/containers/systemd/`, runs `systemctl --user daemon-reload`, then
`systemctl --user restart` for each generated service — `start_first`
dependencies first.

Service names follow Podman's generator: `foo.container` → `foo.service`,
`bar.pod` → `bar-pod.service`, `baz.kube` → `baz.service`. Volume, network,
image and build units generate no long-running service; systemd activates them
as dependencies.

Because that install directory is shared across projects, each deploy **converges**
its own unit set: the agent records the files it installed in
`~/.config/containers/systemd/.<project>.steward-units.json`, and on the next
deploy stops and deletes any unit it previously owned that no longer exists in
the checkout. Renaming or deleting a unit — or rolling back away from a commit
that added one — therefore doesn't leave an orphaned service running. This is the
quadlet equivalent of compose's `--remove-orphans`.

## Contexts and timeouts

| Phase | Deadline | Source |
| --- | --- | --- |
| checkout | 10 min | `checkoutTimeout` constant — the manifest isn't readable yet |
| secrets → verify | `deployment.timeout` (default 10m) | manifest |
| each health attempt | `health_check.timeout` (default 30s) | manifest, nested inside the above |
| log capture | 30 s | `logCaptureGrace` constant, detached — runs after the deployment context has usually expired |
| rollback | 5 min | `rollbackGrace` constant, fresh deadline (covers the restored commit's health check too) |
| notify | 30 s | `notifyGrace` constant, context detached from the deployment |
| shutdown drain | `shutdown_grace` (default 2m) | agent config |

Rollback deliberately gets a *fresh* deadline: the most common reason to roll
back is that the deployment deadline already expired. It still derives from the
dispatcher's base context, so a forced shutdown aborts it.

Cancelling a context kills the subprocess: every external command runs under
`exec.CommandContext` with a 10-second `WaitDelay`, so a process that ignores
the kill can't hang the agent.

## Failure matrix

| Failure | Rollback? | Commit status | Exit path |
| --- | --- | --- | --- |
| Bad HMAC signature | — | none | `401`, nothing enqueued |
| Repo/branch not configured | — | none | `200 ignored` |
| `git fetch`/`checkout` fails | no | `error` | notify |
| SHA not on `origin/<branch>` (ancestry check) | no | `error` | notify |
| `.steward.yaml` missing or invalid | no | `error` | notify |
| `sops --decrypt` fails | no | `error` | notify |
| Backup command exits non-zero | no | `error` | notify |
| `deploy_image` outside the project's `images` allowlist | no | `error` | notify |
| `podman pull`/pin of the delivered image fails | no | `error` | notify |
| `compose up` / `systemctl restart` fails | **yes** | `failure` | rollback → notify |
| Health check exhausts retries | **yes** | `failure` | rollback → notify |
| Panic anywhere in the pipeline | no | `error` | notify |
| Rollback itself fails | — | `failure` + `(ROLLBACK FAILED)` | notify, high-priority ntfy |
| Rollback deploys but stays unhealthy | — | `failure` + `(ROLLBACK FAILED)` | notify — the host is serving a broken app |
| Superseded by a newer push while queued | never ran | `error` ("superseded by …") | no push notification |

`failure` means "your deployment was bad"; `error` means "the agent couldn't get
far enough to find out".

## Persistent state

```
<data_dir>/
  steward.lock                 flock(2), holder's PID; never deleted on release
  checkouts/<project>/          detached-HEAD working copy
  state/<project>.json          {"last_good_sha": "...", "deployed_at": "..."}
```

`state/<project>.json` is written atomically (temp file + rename) *only after a
deployment passes its health check*, so a crash can never promote a broken
commit to rollback target.

Because the project name keys the checkout directory, the state file, and the
dispatcher queue, the config loader rejects two projects that resolve to the same
name or directory — otherwise a staging deploy could overwrite production's
rollback target.

## Environment exported to child processes

Every subprocess the lifecycle spawns — compose, `systemctl`, backup commands,
command health checks — inherits the agent's environment plus:

| Variable | Example |
| --- | --- |
| `STEWARD_PROJECT` | `my-awesome-app` |
| `STEWARD_SHA` | `9f2a1c…` (40 chars) |
| `STEWARD_SHORT_SHA` | `9f2a1c3` |
| `STEWARD_REF` | `refs/heads/main` |
| `STEWARD_STACK` | `docker-compose` |
| `STEWARD_ENVIRONMENT` | `production` |
| `STEWARD_BACKUP_STAMP` | `20260710T013655Z-9f2a1c3` (backup commands only) |

Compose substitutes variables from the process environment, which makes this the
supported way to pin an image to the commit being deployed:

```yaml
services:
  app:
    image: ghcr.io/alex-savin/my-awesome-app:sha-${STEWARD_SHA}
```

During **rollback** these variables describe the commit being *restored*, so the
previous version's compose file resolves to the image it originally ran.

Quadlet unit files are static — systemd does not expand environment variables in
`Image=`. For quadlet stacks, pin with a mutable tag plus `Pull=newer` in the
`.container` unit, or commit the tag. See [`podman-systemd.unit(5)`](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html).
