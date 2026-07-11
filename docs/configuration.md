# Configuration reference

Two files, with different lifetimes and different owners:

| File | Lives on | Read | Owned by |
| --- | --- | --- | --- |
| `config.yaml` | the deploy host, e.g. `/etc/steward/` | once, at startup | the homelab operator |
| `.steward.yaml` | the repo root, committed | on every deployment, from the checkout | the application repo |

Unknown keys are rejected in both files. A typo fails loudly at startup (agent
config) or fails the deployment with `manifest` as the failing step, rather than
silently disabling a feature.

---

## Agent config (`config.yaml`)

Passed with `--config` (default `/etc/steward/config.yaml`). Full example:
[`configs/config.example.yaml`](../configs/config.example.yaml).

### Top level

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `listen` | string | `127.0.0.1:8484` | Bind address. Keep it on loopback and let Nginx terminate TLS. |
| `trust_proxy` | bool | `false` | Read the client IP from `X-Forwarded-For` (rightmost entry) / `X-Real-Ip`. Logging only. |
| `data_dir` | path | `/var/lib/steward` | Holds `checkouts/` and `state/`. `~` is expanded. |
| `lock_file` | path | `<data_dir>/steward.lock` | `flock(2)` single-instance lock. |
| `shutdown_grace` | duration | `2m` | How long in-flight deployments may finish after `SIGTERM`. |

### `webhook`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `secret` | string | — | Inline HMAC secret. Prefer `secret_file`. |
| `secret_file` | path | — | File containing the secret; trailing newline trimmed. |
| `path` | string | `/webhook/github` | Must start with `/`. |

The secret is resolved in order: `secret`, then `secret_file`, then
`$STEWARD_WEBHOOK_SECRET`. It is **required** — the agent refuses to start
without one. Generate with `openssl rand -hex 32`.

### `github`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `token` | string | — | PAT with the `repo:status` scope. Prefer `token_file`. |
| `token_file` | path | — | File containing the token. |
| `api_url` | URL | `https://api.github.com` | Change for GitHub Enterprise. |

Resolved in order: `token`, `token_file`, `$STEWARD_GITHUB_TOKEN`,
`$GITHUB_TOKEN`. **Optional** — with no token, commit statuses are skipped and
everything else works.

### `telegram`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `token` | string | — | Bot token from [@BotFather](https://t.me/BotFather). Prefer `token_file`. |
| `token_file` | path | — | File containing the bot token. |
| `api_server` | URL | `https://api.telegram.org` | Point at a self-hosted [Bot API server](https://github.com/tdlib/telegram-bot-api). |

Resolved in order: `token`, `token_file`, `$STEWARD_TELEGRAM_TOKEN`.
**Optional** — without it, Telegram notifications are skipped. The token's shape
(`<bot_id>:<35 chars>`) is validated at startup, so a typo fails loudly instead of
silently dropping every notification; the error never echoes the token.

The token lives here, agent-side, and **not** in `.steward.yaml` — that manifest
is committed to the application repo, and a bot token is a credential. Manifests
only name the *chat*.

### `defaults`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `deploy_mode` | `queue` \| `reject` | `queue` | Used only for a project with no checkout yet (first deploy). Afterwards the manifest governs. |
| `max_queue` | int | — | **Deprecated, ignored.** Queue mode coalesces to the newest pending job, so a project never queues more than one. Still accepted so existing configs load. |

### `projects[]`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `repo` | string | **required** unless `path` set | `owner/name`, matched case-insensitively against `repository.full_name`. |
| `branch` | string | `main` | Deploy on pushes to this branch. |
| `name` | string | the repo name | Keys the checkout dir, state file, and dispatcher queue. Must be unique. Required for a `path` project. |
| `clone_url` | string | `https://github.com/<repo>.git` | Use SSH for private repos. Mutually exclusive with `path`. |
| `dir` | path | `<data_dir>/checkouts/<name>` | Must be unique across projects. |
| `manifest` | path | `.steward.yaml` | Manifest path within the source. |
| `path` | path | — | **Local project:** read the manifest + units from this absolute dir, no git clone. See below. |

**Local (image-centric) projects.** Set `path` instead of `clone_url` to deploy
from a directory already on the host — no repo clone. These projects are driven by
an **image-centric trigger** (`deploy_image`), not a git push; a plain push to a
local project is ignored. Match is by `name`:

```yaml
projects:
  - name: firecrew
    path: /opt/firecrew/src   # holds .steward.yaml + deploy/quadlet
```

Trigger a deploy with an HMAC-signed POST carrying the image (see
[github-actions.md](github-actions.md#reference-the-dispatch-contract)):

```jsonc
{ "project": "firecrew", "deploy_image": "ghcr.io/alex-savin/go-nttd:sha-abc" }
```

The same repo on two branches needs two entries with **distinct `name` values** —
the loader rejects a collision rather than letting staging clobber production's
rollback state:

```yaml
projects:
  - repo: alex-savin/my-awesome-app
    branch: main
    name: my-awesome-app
  - repo: alex-savin/my-awesome-app
    branch: staging
    name: my-awesome-app-staging
```

---

## Deployment manifest (`.steward.yaml`)

Committed to the root of each deployed repository. Full example:
[`configs/steward.example.yaml`](../configs/steward.example.yaml).

```yaml
project: my-awesome-app
environment: production
stack: quadlet            # or docker-compose
```

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `project` | string | **required** | Identifies the project in logs, notifications, and the quadlet ownership file. |
| `environment` | string | — | Free-form label; surfaced in notifications and `STEWARD_ENVIRONMENT`. |
| `stack` | `quadlet` \| `docker-compose` | **required** | Selects the driver. |

### `deployment`

```yaml
deployment:
  mode: queue    # queue | reject
  timeout: 10m
```

`timeout` bounds steps 1–4 together (secrets, backup, deploy, verify). It does
**not** cover the checkout, which has its own 10-minute deadline, nor rollback,
which gets a fresh 5-minute one (including the restored commit's health check),
nor the 30-second log capture after a failure.

`mode` only takes effect once the agent has a checkout of the repo — see
[the concurrency model](architecture.md#concurrency-model). In `queue` mode the
queue holds at most one pending job: a newer push supersedes the one waiting.

### `image` (image-centric deploys)

```yaml
image:
  local_tag: localhost/go-nttd:live
```

Optional. When a deploy carries an image (a `deploy_image` trigger), Steward
`podman pull`s it and re-tags `local_tag` to it before restarting — because a
quadlet unit's `Image=` line is static, this is how a CD-delivered image reaches
the running unit. Point the unit's `Image=` at `local_tag`. Rollback re-pins the
previous image (stored as `last_good_image`), so don't aggressively prune old
images.

For a **compose** stack, omit this and reference `${STEWARD_IMAGE}` in the compose
file instead — Steward exports it and `compose up` pulls it.

### `secrets`

```yaml
secrets:
  provider: sops
  key_file: /opt/steward/keys/age.txt
  files:
    - source: secrets/production.enc.env
      destination: .env
```

| Key | Notes |
| --- | --- |
| `provider` | Only `sops` is supported. Omit the block to skip step 1 entirely. |
| `key_file` | age identity on the **deploy host**, exported as `SOPS_AGE_KEY_FILE`. Required when `provider: sops`. |
| `files[].source` | Encrypted file, relative to the repo root. |
| `files[].destination` | Plaintext output, relative to the repo root, written `0600`. |

Both paths must stay inside the checkout — absolute paths and `..` are rejected at
manifest load. The agent requires the `sops` binary on `PATH`.

### `backup`

```yaml
backup:
  retention:
    keep_last: 3
  volumes:
    - source: /var/lib/homelab/app_data
      destination: /opt/backups/my-awesome-app/
  commands:
    - docker exec db-1 pg_dump -U postgres myapp > /var/lib/homelab/app_data/dump.sql
```

| Key | Default | Notes |
| --- | --- | --- |
| `retention.keep_last` | `3` | Snapshots kept per **destination** directory. Must be ≥ 1. |
| `volumes[].source` | — | File or directory to snapshot. A missing source is warned about and skipped (first deploy). |
| `volumes[].destination` | — | Snapshots land in `<destination>/<UTC timestamp>-<short sha>/`. |
| `commands[]` | — | Shell lines run via `/bin/sh -c` in the checkout directory. A non-zero exit aborts the deployment before anything is touched. |

**Ordering:** commands run **before** volume copies. That is what makes the
example above work — the dump is written into a directory that is itself a
backup volume, so it is captured by the same snapshot. A dump written to `/tmp`
is never backed up and never rotated.

Rotation only deletes directories whose names match the snapshot pattern
(`20260710T013655Z-9f2a1c3`). Rotation failures are logged, never fatal.

Restores use `rsync -a --delete` when rsync is present (exact restore), otherwise
`cp -a` (overlay). Single-file sources are copied directly.

### `dependencies`

```yaml
dependencies:
  - name: database
    type: postgres
    start_first: true
```

`name` must match the compose **service name**, or the quadlet **unit base name**
(`database.container` → `database`). `type` is informational. Only
`start_first: true` changes behavior: that service is started before the rest of
the stack. Order among several `start_first` entries follows manifest order.

### `health_check`

```yaml
health_check:
  type: http                              # http | command
  endpoint: http://localhost:8080/healthz # type: http
  command: 'curl -fsS localhost:8080/z'   # type: command
  timeout: 30s                            # per attempt
  retries: 3
```

Omit the block and a deployment is successful as soon as the stack starts — no
rollback will ever trigger from verification. `http` passes on 2xx; `command`
passes on exit 0 and runs in the checkout directory. Backoff between attempts is
linear: 2s, 4s, 6s…

This block does double duty: after a rollback the agent re-runs the **restored
commit's** health check (the one committed alongside *that* SHA, not this one) to
confirm the previous version actually came back up. With no health check, a
rollback is assumed to have worked the moment the containers start.

Total worst case is roughly `retries × timeout + backoff`, and it must fit inside
`deployment.timeout` along with everything else.

### `notifications`

```yaml
notifications:
  ntfy:
    topic: homelab_deployments
    server: https://ntfy.mydomain.com   # default https://ntfy.sh
    token: tk_...                       # optional bearer token
  telegram:
    chat_id: "-1001234567890"           # or "@homelab_deploys"; quote it
    message_thread_id: 42               # optional: a forum supergroup topic
    silent: true                        # optional: deliver without a sound
```

Both channels are optional and independent; configure either, both, or neither.

Success sends ntfy priority `default`; failure sends priority `high` and states
whether the rollback succeeded. Notification failures are logged and never fail a
deployment.

| `telegram` key | Type | Notes |
| --- | --- | --- |
| `chat_id` | string | **Required.** A numeric id (negative for groups/channels) or an `@channelusername`. Always quote it — YAML would otherwise read `-100…` as a number. |
| `message_thread_id` | int | Target a topic inside a forum supergroup. |
| `silent` | bool | Send with `disable_notification`. |

There is deliberately **no `token` key here.** The bot token is agent-side
([`telegram`](#telegram) in `config.yaml`); a manifest committed to an application
repo must not carry credentials. A `token:` in this block is a hard error.

Messages are sent in Telegram's HTML parse mode. Everything interpolated — commit
subjects, command output, error text — is escaped, so a commit message containing
`<script>` or `&` cannot break the message or inject markup. The short SHA links
to the commit on github.com (omitted for GitHub Enterprise, whose web host cannot
be inferred from `github.api_url`).

Commit statuses are separate and need no manifest configuration — they are posted
whenever the agent has a GitHub token, under the context
`steward/<project name>`.

### `compose` / `quadlet`

```yaml
compose:
  file: docker-compose.yml   # default: first of compose.yaml, compose.yml,
                             # docker-compose.yml, docker-compose.yaml
quadlet:
  dir: quadlets              # repo dir holding the unit files
```

---

## Durations

Anywhere a duration appears (`timeout`, `shutdown_grace`) the value is parsed by
Go's `time.ParseDuration`: `30s`, `10m`, `1h30m`. A bare number is invalid.

## Environment variables

| Variable | Purpose |
| --- | --- |
| `STEWARD_WEBHOOK_SECRET` | Fallback for `webhook.secret` |
| `STEWARD_GITHUB_TOKEN` | Fallback for `github.token` |
| `GITHUB_TOKEN` | Last-resort fallback for the GitHub token |
| `STEWARD_TELEGRAM_TOKEN` | Fallback for `telegram.token` |

Variables exported *to* deployments are listed in
[architecture.md](architecture.md#environment-exported-to-child-processes).
