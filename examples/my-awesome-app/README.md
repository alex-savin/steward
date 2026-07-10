# Example application repository

What a repo deployed by Steward looks like. Copy the pieces you need
into your own application repo — none of this belongs in the agent's repo.

```
.steward.yaml                     the deployment manifest the agent reads
.sops.yaml                         which age key encrypts secrets/*.enc.env
docker-compose.yml                 stack: docker-compose
quadlets/app.container             stack: quadlet   (use one or the other)
quadlets/database.container
secrets/production.enc.env         SOPS-encrypted; safe to commit
.github/workflows/cd.yml           test → build → push image → dispatch deploy
```

The manifest here pins the app image to `sha-${STEWARD_SHA}`, the commit the
agent is deploying. See
[docs/github-actions.md § Pinning the image to the commit](../../docs/github-actions.md#pinning-the-image-to-the-commit).

The CD workflow runs `steward validate .steward.yaml` in its test job, so
a manifest typo fails in CI rather than by aborting a real deployment on the host.

Before this works, on the agent host:

```yaml
# /etc/steward/config.yaml
projects:
  - repo: alex-savin/my-awesome-app
    branch: main
```

and in the app repo's GitHub secrets: `STEWARD_URL`, `STEWARD_WEBHOOK_SECRET`.
