#!/usr/bin/env bash
#
# steward-dispatch.sh — trigger a Steward deployment from CI.
#
# Builds a GitHub-push-shaped payload, signs it with HMAC-SHA256 exactly as
# GitHub does (X-Hub-Signature-256), and POSTs it to a Steward agent.
# Use this when you want CI to gate deployments (tests green, image pushed)
# instead of wiring the repository webhook straight to the agent.
#
# Required environment:
#   STEWARD_URL              https://deploy.example.com/webhook/github
#   STEWARD_WEBHOOK_SECRET   the agent's webhook.secret
#   GITHUB_REPOSITORY         owner/name   (set automatically by Actions)
#   GITHUB_SHA                commit to deploy (set automatically by Actions)
#   GITHUB_REF                refs/heads/main (set automatically by Actions)
#
# Optional:
#   STEWARD_MESSAGE          one-line description shown in logs/notifications
#   --wait                    poll the commit status until the deploy finishes
#                             (needs `gh` and GH_TOKEN/GITHUB_TOKEN; `gh api --jq`
#                             uses gh's embedded engine, no standalone jq needed)
#                             Note: if a newer push supersedes this commit while
#                             it is queued, the agent posts `error` on this SHA
#                             ("superseded; not deployed") and --wait exits 3.
#   --status-context <ctx>    commit status context to poll
#                             (default: steward/<repo name>)
#   --timeout <seconds>       how long --wait polls before failing (default 900)
#
# Exit codes: 0 accepted (and, with --wait, deployment succeeded)
#             1 usage/transport error   2 rejected by the agent
#             3 deployment failed or timed out (only with --wait)
set -euo pipefail

WAIT=false
STATUS_CONTEXT=""
TIMEOUT=900

while [[ $# -gt 0 ]]; do
  case "$1" in
    --wait)           WAIT=true; shift ;;
    --status-context) STATUS_CONTEXT="$2"; shift 2 ;;
    --timeout)        TIMEOUT="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

: "${STEWARD_URL:?STEWARD_URL is required}"
: "${STEWARD_WEBHOOK_SECRET:?STEWARD_WEBHOOK_SECRET is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${GITHUB_REF:?GITHUB_REF is required}"

# The agent only reads these fields, but they must be shaped like a push event.
# Keep the message on one line and free of quotes/backslashes so the payload
# stays valid JSON without a jq dependency.
message="$(printf '%s' "${STEWARD_MESSAGE:-deployed by GitHub Actions}" \
  | tr -d '\r' | head -n 1 | tr '"\\' "''" )"

payload=$(printf '{"ref":"%s","after":"%s","deleted":false,"repository":{"full_name":"%s"},"pusher":{"name":"%s"},"head_commit":{"message":"%s"}}' \
  "$GITHUB_REF" "$GITHUB_SHA" "$GITHUB_REPOSITORY" "${GITHUB_ACTOR:-github-actions}" "$message")

# HMAC-SHA256 over the exact bytes we send. `awk '{print $NF}'` tolerates both
# "(stdin)= <hex>" (OpenSSL 1.x) and "SHA2-256(stdin)= <hex>" (OpenSSL 3.x).
# Note: the secret appears in this process's argv; acceptable on ephemeral CI
# runners, and the alternative openssl invocations have the same property.
signature=$(printf '%s' "$payload" \
  | openssl dgst -sha256 -hmac "$STEWARD_WEBHOOK_SECRET" -hex \
  | awk '{print $NF}')

echo "▸ dispatching ${GITHUB_REPOSITORY}@${GITHUB_SHA:0:7} → ${STEWARD_URL}"

response=$(mktemp)
trap 'rm -f "$response"' EXIT

code=$(curl -sS --fail-with-body -o "$response" -w '%{http_code}' \
  -X POST "$STEWARD_URL" \
  -H 'Content-Type: application/json' \
  -H 'X-GitHub-Event: push' \
  -H "X-GitHub-Delivery: gha-${GITHUB_RUN_ID:-manual}-${GITHUB_RUN_ATTEMPT:-1}" \
  -H "X-Hub-Signature-256: sha256=${signature}" \
  --data-binary "$payload" || true)

body=$(cat "$response")
echo "  agent responded ${code}: ${body}"

case "$code" in
  202) ;;   # started, or queued as the only pending job for this project
  200) echo "✗ agent ignored the event (no project configured for this repo/branch?)" >&2; exit 2 ;;
  401) echo "✗ signature rejected — STEWARD_WEBHOOK_SECRET does not match the agent" >&2; exit 2 ;;
  409) echo "✗ a deployment is already running and the manifest sets deployment.mode: reject" >&2; exit 2 ;;
  # Unreachable since queue mode began coalescing to a single pending job;
  # kept so an older agent still reports something useful.
  429) echo "✗ the agent's deployment queue is full" >&2; exit 2 ;;
  503) echo "✗ the agent is shutting down" >&2; exit 2 ;;
  *)   echo "✗ unexpected response from agent" >&2; exit 1 ;;
esac

if [[ "$WAIT" != true ]]; then
  echo "✓ accepted (202). The agent reports the result as a commit status on ${GITHUB_SHA:0:7}."
  exit 0
fi

# --wait: the agent posts pending → success/failure/error to the Commit
# Statuses API. Poll that context rather than guessing a fixed sleep.
if [[ -z "$STATUS_CONTEXT" ]]; then
  STATUS_CONTEXT="steward/${GITHUB_REPOSITORY##*/}"
fi
echo "▸ waiting for commit status '${STATUS_CONTEXT}' (timeout ${TIMEOUT}s)"

deadline=$(( SECONDS + TIMEOUT ))
while (( SECONDS < deadline )); do
  sleep 5
  state=$(gh api "repos/${GITHUB_REPOSITORY}/commits/${GITHUB_SHA}/statuses" \
    --jq "[.[] | select(.context == \"${STATUS_CONTEXT}\")] | first | .state // \"missing\"" 2>/dev/null || echo missing)
  case "$state" in
    success)         echo "✓ deployment succeeded"; exit 0 ;;
    failure|error)   echo "✗ deployment reported ${state}" >&2; exit 3 ;;
    pending|missing) ;;  # keep polling
  esac
done

echo "✗ timed out after ${TIMEOUT}s waiting for '${STATUS_CONTEXT}'" >&2
exit 3
