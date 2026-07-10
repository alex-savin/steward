# GitHub Actions CD pipelines

Steward accepts one thing: an HMAC-signed, push-shaped JSON payload on
`POST /webhook/github`. GitHub's own webhook sends exactly that — and so can a
GitHub Actions job, which is what lets you gate deployments on green tests and a
published image.

- [Choosing a pattern](#choosing-a-pattern)
- [Pattern A — repository webhook (no CI)](#pattern-a--repository-webhook-no-ci)
- [Pattern B — CI-gated deploy (recommended)](#pattern-b--ci-gated-deploy-recommended)
- [Pattern C — self-hosted runner, nothing exposed](#pattern-c--self-hosted-runner-nothing-exposed)
- [Pattern D — fan out to several homelab nodes](#pattern-d--fan-out-to-several-homelab-nodes)
- [Production approvals with Environments](#production-approvals-with-environments)
- [Pinning the image to the commit](#pinning-the-image-to-the-commit)
- [Waiting for the deployment result](#waiting-for-the-deployment-result)
- [Reference: the dispatch contract](#reference-the-dispatch-contract)
- [Troubleshooting](#troubleshooting)

## Choosing a pattern

| | Agent reachable from the internet | CI gates the deploy | Deploys on |
| --- | --- | --- | --- |
| **A** repository webhook | yes | ✗ | every push |
| **B** CI-gated dispatch | yes | ✓ | green build |
| **C** self-hosted runner | **no** | ✓ | green build |
| **D** multi-node fan-out | either | ✓ | green build, N hosts |

Pattern B is the default recommendation. Pattern A deploys a commit whose tests
have not run yet; it is fine for a personal dashboard, not for anything with a
database.

Patterns B, C and D all use the same helper,
[`scripts/steward-dispatch.sh`](../scripts/steward-dispatch.sh), wrapped as a
composite action at [`.github/actions/steward-dispatch`](../.github/actions/steward-dispatch/action.yml).

### Action versions

The workflows below pin these majors (latest as of 2026-07-10):

| Action | Pin |
| --- | --- |
| `actions/checkout` | `v7` |
| `actions/setup-go` | `v6` |
| `docker/setup-qemu-action` | `v4` |
| `docker/setup-buildx-action` | `v4` |
| `docker/login-action` | `v4` |
| `docker/build-push-action` | `v7` |

Every one of those majors runs on the **Node 24** runtime, which requires
**Actions Runner ≥ v2.327.1**. GitHub-hosted runners are always current; a
self-hosted runner ([Pattern C](#pattern-c--self-hosted-runner-nothing-exposed))
that hasn't auto-updated will fail with an unsupported-runtime error.

Let Dependabot keep the pins fresh — `.github/dependabot.yml` in the app repo:

```yaml
version: 2
updates:
  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
```

---

## Pattern A — repository webhook (no CI)

No workflow at all. Point the repo's webhook at the agent
([setup](host-setup.md#configure-the-github-webhook)) and every push to the
configured branch deploys.

The agent still protects you: health check failures roll back automatically.
But a commit that fails its unit tests will be deployed, started, health-checked,
and only *then* rolled back — a visible outage. Prefer B.

---

## Pattern B — CI-gated deploy (recommended)

Test → build → push image → dispatch. The agent's webhook stays enabled in the
sense that the URL is reachable, but **the repository webhook is not configured**;
only this workflow can trigger a deployment.

`.github/workflows/deploy.yml` in the **application** repo:

```yaml
name: CD

on:
  push:
    branches: [main]

concurrency:
  # Never let two deploys of the same branch race; cancel the older run.
  group: cd-${{ github.ref }}
  cancel-in-progress: true

permissions:
  contents: read
  packages: write   # push to GHCR
  statuses: read    # only needed for `wait: true`

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v6
        with:
          go-version: '1.26'
      - run: go test ./...

      # Catch a .steward.yaml typo here, not by failing a real deployment.
      - name: Validate the deployment manifest
        run: |
          curl -sSL -o /tmp/steward \
            https://github.com/alex-savin/steward/releases/latest/download/steward-linux-amd64
          chmod +x /tmp/steward
          /tmp/steward validate .steward.yaml

  build:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7

      - uses: docker/setup-qemu-action@v4
      - uses: docker/setup-buildx-action@v4

      - uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and push
        uses: docker/build-push-action@v7
        with:
          context: .
          # Proxmox nodes are amd64, the Mac Minis are arm64. Build both.
          platforms: linux/amd64,linux/arm64
          push: true
          tags: |
            ghcr.io/${{ github.repository }}:sha-${{ github.sha }}
            ghcr.io/${{ github.repository }}:main
          cache-from: type=gha
          cache-to: type=gha,mode=max

  deploy:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - name: Deploy to homelab
        uses: alex-savin/steward/.github/actions/steward-dispatch@v1
        with:
          url: ${{ secrets.STEWARD_URL }}
          secret: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
          message: ${{ github.event.head_commit.message }}
          wait: 'true'
```

Two repository secrets are required:

| Secret | Value |
| --- | --- |
| `STEWARD_URL` | `https://deploy.example.com/webhook/github` |
| `STEWARD_WEBHOOK_SECRET` | the exact contents of the agent's `webhook.secret` |

Referencing the action by `owner/repo/path@ref` checks out this whole repository
under the hood, so the action finds the `scripts/steward-dispatch.sh` it calls.
If you vendor it into your app repo instead, copy **both**
`.github/actions/steward-dispatch/action.yml` *and* `scripts/steward-dispatch.sh`,
keeping their relative positions.

`@v1` assumes you have pushed this repository to GitHub and tagged it `v1`
(`git tag v1 && git push origin v1`). Until then, use `@main`, or pin to a commit
SHA — the supply-chain-safe choice for a third-party action, and equally valid for
your own.

…or skip the action entirely — it is ~10 lines of `curl`:

```yaml
      - name: Dispatch deployment
        env:
          SECRET: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
        run: |
          payload=$(printf '{"ref":"%s","after":"%s","repository":{"full_name":"%s"},"pusher":{"name":"%s"}}' \
            "$GITHUB_REF" "$GITHUB_SHA" "$GITHUB_REPOSITORY" "$GITHUB_ACTOR")
          sig=$(printf '%s' "$payload" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')
          curl -sS --fail-with-body -X POST "${{ secrets.STEWARD_URL }}" \
            -H 'Content-Type: application/json' \
            -H 'X-GitHub-Event: push' \
            -H "X-Hub-Signature-256: sha256=$sig" \
            --data-binary "$payload"
```

The signature covers the **exact bytes** you send. Build the payload string once
and pass it to both `openssl` and `curl --data-binary`; re-serializing it (with
`jq`, or `-d @file` plus a trailing newline) changes the bytes and yields a 401.

> **Why not `repository_dispatch`?** It would need a second inbound path and its
> own auth. Reusing the push shape means the agent has exactly one authenticated
> entry point, and the payload GitHub sends and the payload CI sends are
> indistinguishable — same validation code, same tests.

---

## Pattern C — self-hosted runner, nothing exposed

The strongest option for a homelab: the agent listens only on loopback, no Nginx
vhost, no inbound firewall rule. A self-hosted runner on the same host (or LAN)
performs the dispatch.

Install a runner on the deploy host, labelled for the node:

```sh
# GitHub repo → Settings → Actions → Runners → New self-hosted runner
./config.sh --url https://github.com/alex-savin/my-awesome-app \
            --token <registration-token> \
            --labels homelab,proxmox-01
```

```yaml
  deploy:
    needs: build
    runs-on: [self-hosted, homelab, proxmox-01]
    steps:
      - name: Deploy to the local agent
        uses: alex-savin/steward/.github/actions/steward-dispatch@v1
        with:
          url: http://127.0.0.1:8484/webhook/github
          secret: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
          wait: 'true'
```

The HMAC secret still matters: it authenticates the *request*, and anything else
on that host could otherwise POST to the loopback port.

Run the runner as a **different user** than `steward`, so a compromised
workflow can't read the agent's age key or GitHub token.

Two things a minimal self-hosted runner often lacks:

- **Runner ≥ v2.327.1.** The action majors pinned above need the Node 24 runtime.
  Check with `./run.sh --version`; runners normally self-update unless you passed
  `--disableupdate`.
- **`gh`, plus a token,** which `wait: 'true'` uses to poll the commit status.
  `gh api --jq` evaluates the filter with its own embedded engine, so a separate
  `jq` package is **not** required. Install `gh`, or drop `wait` and let the agent
  report via commit status and ntfy/Telegram.

---

## Pattern D — fan out to several homelab nodes

A matrix over agents. Each node gets its own URL secret; `fail-fast: false` so
one node failing doesn't cancel the others.

```yaml
  deploy:
    needs: build
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - node: proxmox-01
            url_secret: STEWARD_URL_PROXMOX_01
          - node: mac-mini-01
            url_secret: STEWARD_URL_MAC_MINI_01
          - node: mac-pro
            url_secret: STEWARD_URL_MAC_PRO
    steps:
      - name: Deploy to ${{ matrix.node }}
        uses: alex-savin/steward/.github/actions/steward-dispatch@v1
        with:
          url: ${{ secrets[matrix.url_secret] }}
          secret: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
          message: '${{ github.event.head_commit.message }} → ${{ matrix.node }}'
          wait: 'true'
          # Each agent posts the same commit status context unless its project
          # name differs. Point each job at its own context to avoid confusion.
          status-context: steward/my-awesome-app-${{ matrix.node }}
```

Every agent posts a commit status under `steward/<project name>` — the
`project.name` from *its own* config. Give each node's project a distinct name
(`my-awesome-app-proxmox-01`, …) so the statuses don't overwrite one another,
and match `status-context` to it.

Nodes deploy independently: there is no cross-node rollback. If `mac-pro` fails
its health check it rolls itself back while `proxmox-01` stays on the new commit.
For a lockstep rollout, deploy to one canary node first and gate the rest on it
with `needs:`.

---

## Production approvals with Environments

Steward has no notion of approval — it deploys what it is told. Put the
gate in Actions, where it belongs:

1. Repo → **Settings → Environments → New environment** → `production`.
2. Add yourself under **Required reviewers**.
3. Move `STEWARD_URL` / `STEWARD_WEBHOOK_SECRET` into that environment's
   secrets (they then exist *only* for jobs that name it).

```yaml
  deploy-staging:
    needs: build
    runs-on: ubuntu-latest
    environment: staging
    steps:
      - uses: alex-savin/steward/.github/actions/steward-dispatch@v1
        with:
          url: ${{ secrets.STEWARD_URL }}
          secret: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
          ref: refs/heads/main
          wait: 'true'

  deploy-production:
    needs: deploy-staging          # only after staging is healthy
    runs-on: ubuntu-latest
    environment: production        # ⏸ waits here for a human
    steps:
      - uses: alex-savin/steward/.github/actions/steward-dispatch@v1
        with:
          url: ${{ secrets.STEWARD_URL }}
          secret: ${{ secrets.STEWARD_WEBHOOK_SECRET }}
          wait: 'true'
```

The `ref` input must match the `branch` of a project configured on the target
agent, otherwise the agent answers `200 {"status":"ignored"}` and the script
fails with exit 2. Two agents (or two projects on one agent) with different
branches is the usual staging/production split — see
[projects\[\]](configuration.md#projects).

---

## Pinning the image to the commit

CI pushes `ghcr.io/owner/app:sha-<sha>`. The agent checks out that same commit
and exports `STEWARD_SHA` into the environment of every process it spawns.
Compose substitutes from the process environment, so the repo's compose file can
name the exact image without CI rewriting any file:

```yaml
# docker-compose.yml, committed to the app repo
services:
  app:
    image: ghcr.io/alex-savin/my-awesome-app:sha-${STEWARD_SHA}
    env_file: [.env]          # written by the SOPS step
    ports: ['8080:8080']
    depends_on: [database]

  database:
    image: postgres:16-alpine
    volumes: ['/var/lib/homelab/app_data:/var/lib/postgresql/data']
```

Immutable tags matter: `docker compose up -d` only pulls an image it doesn't
already have locally. With `:latest` it would happily restart the stale image it
pulled last week. A `sha-` tag is never present locally, so it is always pulled.
If you must use a moving tag, add `pull_policy: always`.

Rollback re-runs the previous commit's compose file with `STEWARD_SHA` set to
the **last known-good SHA**, so it resolves to the image that commit originally
ran. This is the whole reason the variable exists.

**Quadlets** are static unit files; systemd does not expand `${STEWARD_SHA}` in
`Image=`. Use a moving tag plus `Pull=newer`:

```ini
# quadlets/app.container
[Container]
Image=ghcr.io/alex-savin/my-awesome-app:main
Pull=newer
PublishPort=8080:8080
EnvironmentFile=%h/checkouts/my-awesome-app/.env

[Service]
Restart=always

[Install]
WantedBy=default.target
```

`Pull=newer` requires a reasonably recent Podman; check
`man podman-systemd.unit`. The trade-off is real: quadlet rollback restores the
previous *unit file*, but if that unit names a moving tag, `Pull=newer` may pull
the same broken image. Pin the tag in the unit file (and let CI open a PR that
bumps it) when you need rollback to be exact.

## Waiting for the deployment result

`wait: 'true'` makes the job block until the agent posts a terminal commit status
on the SHA, so a red deployment turns the workflow red.

It works by polling `GET /repos/{owner}/{repo}/commits/{sha}/statuses` for the
context `steward/<project name>` — which means:

- the agent needs a **GitHub token** ([`github.token`](configuration.md#github)),
  otherwise it never posts a status and the job times out;
- the workflow needs `statuses: read` permission (or `GH_TOKEN` with it);
- the default context is `steward/<repo name>`. If the agent's project
  `name` differs from the repo name, pass `status-context:` explicitly.

Without `wait`, the step succeeds as soon as the agent answers `202` and the
deployment's outcome shows up as a commit status on the commit (and, if
configured, an ntfy or Telegram notification) a minute later.

**`wait` and superseded commits.** If your commit is still queued when a newer
push arrives, the agent supersedes it and posts `error` —
"superseded by `<sha>`; not deployed" — on *your* SHA. `--wait` sees that
terminal status and exits 3, turning the run red. That is accurate: your commit
never deployed. In practice the recommended workflow already sets
`concurrency: cancel-in-progress: true`, so the older run is usually cancelled
before it notices. If you would rather a superseded run go green, drop `wait`
and let the newest deployment's status speak for the branch.

Give `timeout` room for a rollback: worst case is
`deployment.timeout` + 5 minutes of rollback grace. The default is 900 s.

## Reference: the dispatch contract

The agent reads exactly these fields:

```jsonc
{
  "ref": "refs/heads/main",           // branch must match the project's `branch`
  "after": "9f2a1c…",                 // 40-char SHA, must exist on the origin
  "deleted": false,                   // true → ignored
  "repository": { "full_name": "alex-savin/my-awesome-app" },
  "pusher":     { "name": "alex-savin" },        // logging only
  "head_commit": { "message": "Fix the thing" }  // logging only; may be omitted
}
```

Headers the agent enforces:

| Header | Value |
| --- | --- |
| `X-GitHub-Event` | `push` (`ping` answers `200 pong`; anything else is ignored) |
| `X-Hub-Signature-256` | `sha256=` + hex HMAC-SHA256 of the raw body, keyed by the webhook secret |

Send `Content-Type: application/json` too. The agent doesn't check it — it
verifies the HMAC over the raw bytes and then unmarshals them — but GitHub's own
webhook must be configured with it, because the `application/x-www-form-urlencoded`
setting sends the JSON form-encoded, which changes the bytes and fails the HMAC.

Responses, and what `steward-dispatch.sh` does with them:

| Code | Meaning | Script exit |
| --- | --- | --- |
| `202` `started` | Nothing was running; the deployment began | 0 |
| `202` `queued` | A deploy is running; this becomes the **only** pending job, superseding any earlier one | 0 |
| `200` | Ignored — ping, non-push event, unknown repo/branch, branch deletion | 2 |
| `401` | Signature missing or wrong | 2 |
| `409` | Busy, and the manifest says `deployment.mode: reject` | 2 |
| `503` | The agent is shutting down | 2 |

A `429` was previously returned when a project's queue filled up. Queue mode now
coalesces to a single pending job, so it can no longer happen; the script still
recognises the code.

The commit must already exist on the origin — the agent fetches it. In a normal
`on: push` workflow it does. If you dispatch from a `pull_request` event,
`github.sha` is a synthetic merge commit that the agent cannot fetch; use
`github.event.pull_request.head.sha` and make sure the branch is pushed.

## Troubleshooting

**`401 invalid signature`** — the bytes signed differ from the bytes sent, or the
secret differs. Reproduce locally against the real payload:

```sh
payload='{"ref":"refs/heads/main","after":"'"$(git rev-parse HEAD)"'","repository":{"full_name":"alex-savin/my-awesome-app"}}'
printf '%s' "$payload" | openssl dgst -sha256 -hmac "$(sudo cat /etc/steward/webhook.secret)" -hex
```

Common causes: a trailing newline (`echo` instead of `printf '%s'`), a webhook
configured with content type `application/x-www-form-urlencoded`, or a secret
copied with a trailing newline into the GitHub secret box.

**`200 {"status":"ignored"}`** — the `ref` or `repository.full_name` doesn't match
any configured project. Check `branch:` on the agent against the branch you're
deploying; `refs/tags/v1` never matches.

**`409` on the first deploy of a project** — impossible; the *second* is the one
that can 409. Until a checkout exists the agent can't read `deployment.mode` and
falls back to `defaults.deploy_mode`. Set that to `reject` too if you depend on it.

**The job passes but nothing changed on the host** — the agent deployed a commit
whose compose file pins a moving tag that was already present locally. Use
`sha-${STEWARD_SHA}` or `pull_policy: always`.

**`wait` times out** — the agent has no GitHub token, so it never posted a
status; or the project name differs from the repo name and the default
`status-context` is wrong. `curl -s http://agent:8484/status` shows whether the
deployment is still running.

**Image pulls fail on the host** — the *agent's* user, not the runner, pulls the
image. `sudo -u steward docker pull ghcr.io/...` to reproduce, then
[log in](host-setup.md#container-registry-access) or make the package public.

Agent-side diagnosis lives in [operations.md](operations.md).
