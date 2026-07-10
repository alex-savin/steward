# Operations

Running Steward day to day: endpoints, logs, manual recovery, and the
security model you are inheriting.

- [HTTP endpoints](#http-endpoints)
- [Reading the logs](#reading-the-logs)
- [Manual operations](#manual-operations)
- [Restoring a backup by hand](#restoring-a-backup-by-hand)
- [Upgrading the agent](#upgrading-the-agent)
- [Troubleshooting](#troubleshooting)
- [Security model](#security-model)

## HTTP endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/webhook/github` (configurable) | HMAC-SHA256 | Receive push events |
| `GET` | `/healthz` | none | Liveness: `{"status":"ok"}` |
| `GET` | `/status` | none | Per-project queue snapshot |

`/status` returns what is running and what is queued, per project:

```jsonc
{
  "my-awesome-app": {
    "mode": "queue",
    "running": {
      "id": "78ac08feadfc",
      "sha": "9f2a1c3…",
      "ref": "refs/heads/main",
      "received_at": "2026-07-10T01:36:55Z",
      "step": "verify",
      "step_since": "2026-07-10T01:37:10Z"
    },
    "queued": [
      { "id": "f5986ab16d9e", "sha": "6b96577…", "ref": "refs/heads/main", "received_at": "…" }
    ]
  }
}
```

An empty object `{}` means no project has deployed since the agent started —
projects appear on first use, not at startup.

`step` is the lifecycle step the running deployment is in (`checkout`, `manifest`,
`secrets`, `backup`, `deploy`, `verify`, `rollback`) and `step_since` is when it
entered it. That is the difference between a slow deploy and a wedged one: a job
that has been in `verify` for ten minutes is stuck; one in `backup` is copying a
large volume.

`queued` holds at most **one** job. Queue mode coalesces — a newer push replaces
the pending one, because only the newest commit is worth deploying.

Neither `/healthz` nor `/status` is authenticated. `/status` leaks project names
and commit SHAs; keep it off the public vhost
([example](host-setup.md#nginx)).

## Reading the logs

Structured `slog` text on stderr; `--debug` adds debug level. Every line for a
deployment carries `project`, `job`, and short `sha`.

```sh
journalctl -u steward -f                      # linux
tail -f /usr/local/var/log/steward.log        # macOS
```

A healthy deployment:

```
level=INFO msg="deployment starting" project=e2e-app job=78ac08feadfc sha=37bebeb ref=refs/heads/main pusher=alex
level=INFO msg="lifecycle step" project=e2e-app job=78ac08feadfc sha=37bebeb step=secrets
level=INFO msg="lifecycle step" project=e2e-app job=78ac08feadfc sha=37bebeb step=backup
level=INFO msg="backing up volume" source=/var/lib/homelab/app_data snapshot=/opt/backups/e2e-app/20260710T013655Z-37bebeb
level=INFO msg="lifecycle step" project=e2e-app job=78ac08feadfc sha=37bebeb step=deploy
level=INFO msg="lifecycle step" project=e2e-app job=78ac08feadfc sha=37bebeb step=verify
level=INFO msg="deployment succeeded" project=e2e-app job=78ac08feadfc sha=37bebeb
```

A failed one, with rollback:

```
level=WARN  msg="health check attempt failed" attempt=1 of=2 error="/bin/sh -c /usr/bin/false: exit status 1: "
level=ERROR msg="lifecycle step failed" step=verify error="health check failed after 2 attempts: …"
level=WARN  msg="rolling back" to=37bebeb
level=INFO msg="restoring volume from backup" snapshot=/opt/backups/…/appdata target=/var/lib/homelab/app_data
level=INFO msg="rollback complete" restored=37bebeb
```

On a failed deploy the agent logs the stack's own output under
`msg="stack logs at failure"` and includes the last few lines in the ntfy and
Telegram notifications, so the push message tells you *why* the container died,
not merely that it did.

Lines worth alerting on: `rollback finished with errors` (the host is in an
unknown state — this now also fires when the restored version deploys but fails
its health check), `another steward instance holds` (a second agent tried to
start), and `dropping queued deployments on shutdown`.

Secrets never appear in logs. `sops` stdout is captured separately from stderr so
plaintext cannot reach an error message; command output is capped at 64 KiB
(tail-truncated) per command. The Telegram bot token is scrubbed from error text
before it is logged — the Bot API carries the token in the request URL, so an
unredacted transport error (`connection refused`, a TLS failure) would otherwise
publish the credential to anyone who can read the agent's logs:

```
level=WARN msg="telegram notification failed" error="… Post \"https://api.telegram.org/bot<redacted>/sendMessage\": …"
```

## Manual operations

**Validate a manifest** before it ever reaches a deploy host — the same parser
the agent uses, so unknown keys, bad durations, and escaping secret paths all
fail here too. Run it in the application repo's CI, next to `go test`:

```sh
steward validate                     # defaults to ./.steward.yaml
steward validate .steward.yaml staging/.steward.yaml
# ✓ .steward.yaml — project "my-awesome-app", stack "quadlet", mode "queue", timeout 10m0s
```

Exit status is non-zero when any manifest is invalid.

**Deploy a specific commit** (replay, or deploy something the webhook missed):

```sh
export STEWARD_URL=http://127.0.0.1:8484/webhook/github
export STEWARD_WEBHOOK_SECRET=$(sudo cat /etc/steward/webhook.secret)
export GITHUB_REPOSITORY=alex-savin/my-awesome-app
export GITHUB_REF=refs/heads/main
export GITHUB_SHA=9f2a1c3...        # full 40-char SHA, already pushed

./scripts/steward-dispatch.sh
```

**Roll back to an older commit** — there is no rollback command, because a
rollback *is* a deployment of the previous commit. Dispatch the SHA you want:

```sh
GITHUB_SHA=$(git rev-parse HEAD~1) ./scripts/steward-dispatch.sh
```

The agent checks that commit out, runs its manifest, and if it passes the health
check it becomes the new last-known-good. This is safer than editing state by
hand, because it exercises the same path as any other deployment.

**Inspect the rollback target:**

```sh
cat /var/lib/steward/state/my-awesome-app.json
# {"last_good_sha": "37bebeb…", "deployed_at": "2026-07-10T01:36:55Z"}
```

**Force a specific rollback target** (only if that commit really is good):

```sh
sudo systemctl stop steward
sudo -u steward tee /var/lib/steward/state/my-awesome-app.json <<< \
  '{"last_good_sha":"37bebeb...","deployed_at":"2026-07-10T00:00:00Z"}'
sudo systemctl start steward
```

**Watch a deployment's progress.** `/status` names the lifecycle step and when it
started, which separates "slow" from "stuck":

```sh
watch -n2 'curl -s http://127.0.0.1:8484/status | jq ".[].running | {sha, step, step_since}"'
```

**Drain before maintenance.** `SIGTERM` stops the listener immediately, then
gives running deployments `shutdown_grace` to finish. A deployment already
*queued* is dropped (it was only acknowledged with `202`, never run) and the
drop is logged. Anything mid-flight when the grace expires is cancelled — its
subprocesses are killed and it rolls back if it had reached the deploy step.

## Restoring a backup by hand

Snapshots are plain directories; nothing proprietary:

```
/opt/backups/my-awesome-app/
  20260708T221501Z-1a2b3c4/app_data/    ← oldest kept
  20260709T104233Z-5d6e7f8/app_data/
  20260710T013655Z-9f2a1c3/app_data/    ← newest
```

```sh
sudo systemctl --user stop my-awesome-app.service     # or: docker compose stop
sudo rsync -a --delete \
  /opt/backups/my-awesome-app/20260709T104233Z-5d6e7f8/app_data/ \
  /var/lib/homelab/app_data/
sudo systemctl --user start my-awesome-app.service
```

Then dispatch the commit that matches that data, so the agent's state and the
disk agree.

Rotation keeps the newest `keep_last` snapshots **per destination directory** and
only ever deletes directories matching `<UTC timestamp>-<short sha>`. Database
dumps written outside a snapshot directory are never rotated — that is your job.

## Upgrading the agent

```sh
make cross
scp dist/steward-linux-amd64 node:/tmp/
ssh node '
  sudo systemctl stop steward &&                     # drains in-flight deploys
  sudo install -m 0755 /tmp/steward-linux-amd64 /usr/local/bin/steward &&
  sudo systemctl start steward &&
  curl -sf http://127.0.0.1:8484/healthz'
```

Nothing to migrate: state is a JSON file per project, checkouts are ordinary git
repos. Downgrades are equally safe. `steward --version` prints the
version, commit, and build date injected by the Makefile.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `another steward instance holds …` | A second agent is running, or a stale process | `ss -lptn 'sport = :8484'`; the flock is released automatically when the holder dies |
| `webhook rejected: bad or missing signature` | Secret mismatch, or body re-serialized before signing | [dispatch contract](github-actions.md#reference-the-dispatch-contract) |
| `no project configured for repo/branch` | `repo`/`branch` mismatch | Compare `config.yaml` with the payload's `repository.full_name` and `ref` |
| `parse …: field X not found in type` | Unknown key in a config or manifest | Fix the typo; unknown keys are rejected on purpose |
| Step `checkout` fails, `Permission denied (publickey)` | Deploy key missing for a private repo | [Git access](host-setup.md#git-access-for-private-repos) |
| Step `secrets` fails, `sops binary not found` | `sops` absent from the agent's `PATH` | Install it; on launchd set `PATH` explicitly in the plist |
| Step `secrets` fails, `no matching creation rule` | The host's age key isn't in the file's recipients | `sops updatekeys secrets/production.enc.env` |
| Step `backup` fails, `permission denied` | systemd `ProtectSystem=strict` without `ReadWritePaths` | Add every backup source/destination to `ReadWritePaths` |
| Step `deploy` fails, `no compose implementation found` | None of `docker`, `docker-compose`, `podman-compose` is on the agent's `PATH` | Install one; on launchd set `PATH` explicitly in the plist |
| Step `deploy` fails, `no compose file in …` | No `compose.yaml`/`compose.yml`/`docker-compose.yml`/`docker-compose.yaml` in the repo root | Add one, or point `compose.file` at yours |
| Step `deploy` fails, `Failed to connect to bus` | Quadlet stack, no user systemd instance | `loginctl enable-linger steward`, run the agent as a user service |
| Step `verify` always fails | Health endpoint not reachable *from the host* | `curl` the endpoint as the agent's user; container ports may not be published |
| `nothing to roll back to` | The first deployment of a project failed | Expected: there is no known-good commit yet. Fix forward |
| `rollback finished with errors` | Restore, redeploy, or the restored version's health check failed | **Investigate immediately** — the host is in an unknown state |
| `rolled-back version did not become healthy` | The previous commit was redeployed but its health check failed | The host is serving a broken app. The environment likely changed underneath it (a dependency, a volume, a port). Fix forward |
| Commit status `error: superseded by <sha>` | A newer push replaced this commit while it was queued | Expected. Only the newest commit deploys; nothing was run for this SHA |
| A CI job with `wait: true` fails on a commit you never pushed twice | Same as above — your commit was superseded | Set `concurrency: cancel-in-progress: true` in the workflow, or drop `wait` |
| Quadlet: an old service keeps running | Ownership file lost or the unit was installed by hand | `systemctl --user stop <unit>` and delete the file from `~/.config/containers/systemd/` |
| `telegram notification failed … chat not found` | The bot was never added to the chat, or `chat_id` is wrong | Add the bot to the group/channel; for a group, read the numeric id from `getUpdates` (it is negative) |
| Log says `manifest configures notifications.telegram but the agent has no bot token` | Manifest asks for Telegram, agent config has no token | Set `telegram.token_file` in `config.yaml` |

## Security model

**What protects the endpoint.** Only the HMAC-SHA256 signature, compared in
constant time. Source IP is never an authorization input; `X-Forwarded-For` is
read (rightmost entry) for log lines alone. Bodies are capped at 25 MB.

**What a push can do.** `backup.commands` and a `command` health check are shell
lines from the repo's committed `.steward.yaml`. Anyone who can push to a
deployed branch can run arbitrary commands as the agent's user. **This is the
intended trust model** — the same one self-hosted CI has — and it is why the
agent must run as a dedicated, unprivileged user, not root. Protect deployed
branches accordingly (branch protection, required reviews).

**What is contained.** Secret `source` and `destination` paths are validated at
manifest load to stay inside the checkout: absolute paths and `..` are rejected,
so a malicious manifest cannot make the agent write `/etc/passwd` via the SOPS
step. Decrypted files are written `0600`. Backup rotation only deletes
directories matching the snapshot naming pattern, so pointing `destination` at
`/` cannot wipe the host. Project names are validated to be plain, non-escaping
path components.

**What is not contained.** Backup `commands` and command health checks are
unrestricted by design; `backup.volumes` sources and destinations are arbitrary
host paths (constrain them with systemd's `ReadWritePaths`); quadlet units are
installed into the agent user's `~/.config/containers/systemd/`, shared with
anything else that user runs.

**Credentials on the host.** The age private key (`secrets.key_file`), the
webhook secret, the GitHub token, and the Telegram bot token should all be `0400`
and owned by the agent user. A Telegram bot token grants full control of the bot,
so it lives in the agent's config and never in a repo-committed `.steward.yaml`;
manifests name only the destination chat. The GitHub token needs `repo:status`
and nothing more — it cannot push
code, and if it leaks the blast radius is a forged commit status. Registry
credentials live in the agent user's Docker/Podman config.

**Single instance.** An `flock(2)` lock file prevents two agents from managing
one host. The kernel releases it if the process dies, so a stale lock cannot
wedge deployments. The file is intentionally never unlinked — deleting it would
allow two fresh instances to lock different inodes at the same path.
