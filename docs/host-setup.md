# Host setup

Installing the agent on a Proxmox node (Linux) or a Mac Mini / Mac Pro (macOS).

- [Build and install the binary](#build-and-install-the-binary)
- [Create the service user and directories](#create-the-service-user-and-directories)
- [SOPS + age](#sops--age)
- [Git access for private repos](#git-access-for-private-repos)
- [Container registry access](#container-registry-access)
- [Run as a service — Linux (systemd)](#run-as-a-service--linux-systemd)
- [Run as a service — macOS (launchd)](#run-as-a-service--macos-launchd)
- [Nginx](#nginx)
- [Configure the GitHub webhook](#configure-the-github-webhook)
- [Verify the install](#verify-the-install)

## Build and install the binary

```sh
make cross          # → dist/steward-{linux,darwin}-{amd64,arm64}
```

Binaries are static (`CGO_ENABLED=0`), so there is nothing to install alongside
them. Copy the one matching the target:

| Host | Artifact |
| --- | --- |
| Proxmox VM / LXC (x86) | `steward-linux-amd64` |
| Proxmox on ARM, Raspberry Pi | `steward-linux-arm64` |
| Intel Mac Pro | `steward-darwin-amd64` |
| Apple Silicon Mac Mini / Studio | `steward-darwin-arm64` |

```sh
scp dist/steward-linux-amd64 proxmox-node:/tmp/
ssh proxmox-node 'sudo install -m 0755 /tmp/steward-linux-amd64 /usr/local/bin/steward'
```

Runtime dependencies on the deploy host: `git`; `sops` if you use encrypted
secrets; `docker` (or `docker-compose` / `podman-compose`) for compose stacks;
Podman ≥ 4.4 with a user systemd instance for quadlet stacks; `rsync` (optional,
gives exact volume restores instead of overlay copies).

## Create the service user and directories

Deployments execute shell commands from the repo's manifest as this user. Give
it exactly the access its containers need and nothing more — never run the agent
as root.

```sh
sudo useradd --system --create-home --shell /bin/bash steward
sudo usermod -aG docker steward          # compose stacks only

sudo mkdir -p /etc/steward /var/lib/steward /opt/backups
sudo chown -R steward:steward /var/lib/steward /opt/backups

openssl rand -hex 32 | sudo tee /etc/steward/webhook.secret >/dev/null
printf '%s' 'ghp_yourtoken' | sudo tee /etc/steward/github.token >/dev/null
sudo chmod 0400 /etc/steward/*.secret /etc/steward/*.token
sudo chown steward /etc/steward/*.secret /etc/steward/*.token
```

The GitHub token needs only the `repo:status` scope (classic) or *Commit
statuses: read and write* (fine-grained). It is optional; without it you lose
commit statuses and nothing else.

Write your [`config.yaml`](configuration.md#agent-config-configyaml) to
`/etc/steward/config.yaml`.

For **quadlet** stacks, the agent must reach a user systemd instance, and that
instance must survive logout:

```sh
sudo loginctl enable-linger steward
```

## SOPS + age

Generate the identity **on the deploy host** — the private key never leaves it:

```sh
sudo -u steward mkdir -p /opt/steward/keys
sudo -u steward age-keygen -o /opt/steward/keys/age.txt
sudo chmod 0400 /opt/steward/keys/age.txt
grep 'public key' /opt/steward/keys/age.txt      # → age1ql3z...
```

In the **application repo**, commit a `.sops.yaml` naming that public key:

```yaml
creation_rules:
  - path_regex: secrets/.*\.enc\.env$
    age: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
```

Then encrypt and commit:

```sh
cat > /tmp/production.env <<'EOF'
DATABASE_URL=postgres://app:s3cr3t@db:5432/myapp
API_KEY=live_abc123
EOF

sops --encrypt /tmp/production.env > secrets/production.enc.env
shred -u /tmp/production.env
git add .sops.yaml secrets/production.enc.env && git commit -m 'Add encrypted production env'
```

The manifest maps it to a plaintext destination inside the checkout:

```yaml
secrets:
  provider: sops
  key_file: /opt/steward/keys/age.txt
  files:
    - source: secrets/production.enc.env
      destination: .env          # written 0600; compose picks it up automatically
```

To let a teammate (or a CI job) re-encrypt, add their age public key to
`creation_rules` and run `sops updatekeys secrets/production.enc.env`.

## Git access for private repos

The agent clones as the service user. For a private repo, add a **read-only
deploy key** and use the SSH clone URL:

```sh
sudo -u steward ssh-keygen -t ed25519 -N '' -f ~steward/.ssh/id_ed25519
sudo -u steward cat ~steward/.ssh/id_ed25519.pub
# → GitHub repo → Settings → Deploy keys → Add (leave "Allow write access" off)

sudo -u steward ssh-keyscan github.com >> ~steward/.ssh/known_hosts
```

```yaml
projects:
  - repo: alex-savin/my-awesome-app
    clone_url: git@github.com:alex-savin/my-awesome-app.git
```

## Container registry access

The agent never logs in for you. For images in GHCR, authenticate **once** as the
service user; Docker and Podman both persist the credential:

```sh
# classic PAT with read:packages
sudo -u steward bash -c 'echo $CR_PAT | docker login ghcr.io -u alex-savin --password-stdin'
# podman equivalent
sudo -u steward bash -c 'echo $CR_PAT | podman login ghcr.io -u alex-savin --password-stdin'
```

Public GHCR packages need no login at all — the simplest option for a homelab.

## Run as a service — Linux (systemd)

`/etc/systemd/system/steward.service`:

```ini
[Unit]
Description=Steward deployment agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=steward
Group=steward
ExecStart=/usr/local/bin/steward --config /etc/steward/config.yaml
Restart=on-failure
RestartSec=5s

# Give in-flight deployments time to finish or roll back before SIGKILL.
# Keep this comfortably above the agent's shutdown_grace.
KillSignal=SIGTERM
TimeoutStopSec=5m

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=false
ReadWritePaths=/var/lib/steward /opt/backups /var/lib/homelab

[Install]
WantedBy=multi-user.target
```

`ReadWritePaths` must list every `backup.volumes` source and destination your
manifests touch, or the backup step fails with a permission error. `PrivateTmp`
is off because backup commands frequently stage dumps in `/tmp`.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now steward
journalctl -u steward -f
```

For **quadlet** stacks the agent talks to its *own* user systemd instance, so it
must run as a user service instead — install the unit at
`~steward/.config/systemd/user/steward.service` (drop the `User=`
/ `Group=` lines), then:

```sh
sudo loginctl enable-linger steward
sudo -u steward XDG_RUNTIME_DIR=/run/user/$(id -u steward) \
  systemctl --user enable --now steward
```

## Run as a service — macOS (launchd)

`/Library/LaunchDaemons/nyc.savin.steward.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>nyc.savin.steward</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/steward</string>
        <string>--config</string>
        <string>/usr/local/etc/steward/config.yaml</string>
    </array>
    <key>UserName</key>
    <string>_steward</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict><key>SuccessfulExit</key><false/></dict>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>/usr/local/var/log/steward.log</string>
    <key>StandardErrorPath</key>
    <string>/usr/local/var/log/steward.log</string>
</dict>
</plist>
```

```sh
sudo launchctl load -w /Library/LaunchDaemons/nyc.savin.steward.plist
tail -f /usr/local/var/log/steward.log
```

Set `PATH` explicitly: launchd daemons do not inherit a login shell, and the
agent shells out to `git`, `docker`, and `sops`. Homebrew lives in
`/opt/homebrew/bin` on Apple Silicon and `/usr/local/bin` on Intel.

Docker Desktop only runs inside a logged-in GUI session. On a headless Mac Mini
running compose stacks, either use [Colima](https://github.com/abiosoft/colima)
as a launchd agent, or run the agent as a *user* LaunchAgent under an
auto-logged-in account.

## Nginx

Steward expects to sit behind a TLS-terminating proxy. GitHub's webhook
sender must reach it, so this vhost is internet-facing — the HMAC signature is
what protects it.

```nginx
server {
    listen 443 ssl http2;
    server_name deploy.example.com;

    ssl_certificate     /etc/letsencrypt/live/deploy.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/deploy.example.com/privkey.pem;

    # GitHub webhook payloads can reach 25 MB.
    client_max_body_size 25m;

    location /webhook/github {
        proxy_pass http://127.0.0.1:8484;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # A deployment is dispatched to the background; the response is immediate.
        proxy_read_timeout 30s;
    }

    # /status exposes project names, commit SHAs and queue depth. Do not
    # publish it: keep it on the LAN, or put auth in front of it.
    location /status {
        allow 192.168.0.0/16;
        allow 10.0.0.0/8;
        deny all;
        proxy_pass http://127.0.0.1:8484;
    }

    location /healthz {
        proxy_pass http://127.0.0.1:8484;
        access_log off;
    }
}
```

Set `trust_proxy: true` in the agent config so log lines carry the real client
IP. The agent reads the **rightmost** `X-Forwarded-For` entry, which is the one
Nginx appends; do not add a second proxy hop without adjusting for it.

If you'd rather not expose the agent at all, drive deployments from a
**self-hosted GitHub Actions runner** on the LAN — see
[github-actions.md](github-actions.md#pattern-c--self-hosted-runner-nothing-exposed).

## Configure the GitHub webhook

Repo → **Settings → Webhooks → Add webhook**:

| Field | Value |
| --- | --- |
| Payload URL | `https://deploy.example.com/webhook/github` |
| Content type | `application/json` |
| Secret | contents of `/etc/steward/webhook.secret` |
| Events | *Just the push event* |

Content type **must** be `application/json`; the agent verifies the HMAC over the
raw JSON body, and `application/x-www-form-urlencoded` sends different bytes.

Skip this entirely if CI will dispatch deployments instead
([Pattern B](github-actions.md#pattern-b--ci-gated-deploy-recommended)).

## Verify the install

```sh
# 1. The agent is up.
curl -s http://127.0.0.1:8484/healthz          # {"status":"ok"}

# 2. Signature validation works (expect 401).
curl -si -X POST http://127.0.0.1:8484/webhook/github \
  -H 'X-GitHub-Event: push' -H 'X-Hub-Signature-256: sha256=bad' -d '{}' | head -1

# 3. A correctly signed ping is accepted (expect {"status":"pong"}).
SECRET=$(sudo cat /etc/steward/webhook.secret)
BODY='{"zen":"Design for failure."}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')
curl -s -X POST http://127.0.0.1:8484/webhook/github \
  -H 'X-GitHub-Event: ping' -H "X-Hub-Signature-256: sha256=$SIG" -d "$BODY"

# 4. Nothing is deploying.
curl -s http://127.0.0.1:8484/status           # {}

# 5. The binary can lint a manifest offline (no config or network needed).
steward validate /path/to/repo/.steward.yaml
```

GitHub's webhook page shows the same thing: **Recent Deliveries** should show a
`200` for the ping it sent when you saved the hook.
