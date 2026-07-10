# Steward documentation

| Document | Read it when |
| --- | --- |
| [architecture.md](architecture.md) | You want to know what happens between `git push` and a running container — the request path, concurrency model, lifecycle steps, timeouts, and failure matrix. |
| [configuration.md](configuration.md) | You are writing `config.yaml` or `.steward.yaml` and want every key, default, and validation rule. |
| [host-setup.md](host-setup.md) | You are installing the agent on a Proxmox node or a Mac: service user, SOPS keys, systemd/launchd, Nginx, the GitHub webhook. |
| [github-actions.md](github-actions.md) | You want CI to gate deployments: four pipeline patterns, image pinning, approvals, and the dispatch contract. |
| [operations.md](operations.md) | Something is broken, or you need to roll back, restore a backup, or upgrade the agent. |

## The 60-second version

1. GitHub (or a GitHub Actions job) POSTs a push event to
   `https://deploy.example.com/webhook/github`, signed with HMAC-SHA256.
2. Nginx forwards it to the agent on loopback. The agent verifies the signature,
   matches the repo and branch to a configured project, and answers **202** —
   the deployment has not started yet.
3. A background worker (one per project, never two at once) checks out the exact
   commit, reads the repo's `.steward.yaml`, and runs:
   **decrypt secrets → back up volumes → deploy → health check**.
   If a newer push arrives while one is running, it replaces the pending job —
   only the newest commit is worth deploying.
4. If the deploy or the health check fails, the agent captures the stack's logs,
   stops it, restores the backup, checks out the last known-good commit,
   redeploys it, and **re-runs that commit's health check** — a rollback that
   comes up broken is reported as such, not as a success.
5. Either way it posts a commit status back to the SHA and pushes an ntfy and/or
   Telegram notification, with the log tail attached on failure.

## Where to start

- Never run it before → [host-setup.md](host-setup.md), then
  [configuration.md](configuration.md).
- Agent is running, want CI → [github-actions.md](github-actions.md).
- Something is on fire → [operations.md § Troubleshooting](operations.md#troubleshooting).
- Checking a manifest before you push it → `steward validate .steward.yaml`.

Worked example of an application repo — compose file, quadlet units, encrypted
secrets, and a CD workflow: [`examples/my-awesome-app/`](../examples/my-awesome-app/).
