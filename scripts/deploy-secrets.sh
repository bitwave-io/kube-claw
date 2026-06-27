#!/usr/bin/env bash
# Deploy kube-claw with Slack + the agent loop wired up, reading tokens from a
# gitignored secrets file (so they never hit the transcript or git history).
#
# Usage:
#   1. Create .secrets.env (gitignored) in the repo root:
#        SLACK_APP_TOKEN=xapp-...        # Socket Mode app-level token
#        SLACK_BOT_TOKEN=xoxb-...        # bot token (chat:write, reactions:write)
#        ANTHROPIC_API_KEY=sk-ant-...    # powers the agent loop
#        ADMIN_PASSWORD=...              # admin dashboard (/ui) basic-auth   [optional]
#   2. ./scripts/deploy-secrets.sh [path-to-secrets-file]
#
# Channels are NOT configured here — add the bot to any channel and it DMs the
# inviter to ask how it should behave (active vs @-only, in-channel vs threads).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${1:-$ROOT/.secrets.env}"
NS="${NS:-claw-system}"
AGENTS_NS="${AGENTS_NS:-claw-agents}"

[ -f "$ENV_FILE" ] || { echo "secrets file not found: $ENV_FILE" >&2; exit 1; }
set -a; . "$ENV_FILE"; set +a

: "${SLACK_APP_TOKEN:?set SLACK_APP_TOKEN in $ENV_FILE}"
: "${SLACK_BOT_TOKEN:?set SLACK_BOT_TOKEN in $ENV_FILE}"

echo "Creating Slack token Secret in $NS..."
kubectl -n "$NS" create secret generic claw-slack-tokens \
  --from-literal=app-token="$SLACK_APP_TOKEN" --from-literal=bot-token="$SLACK_BOT_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  echo "Creating Anthropic key Secret in $AGENTS_NS (run pods) and $NS (router)..."
  for n in "$AGENTS_NS" "$NS"; do
    kubectl -n "$n" create secret generic claw-anthropic-key \
      --from-literal=api-key="$ANTHROPIC_API_KEY" --dry-run=client -o yaml | kubectl apply -f -
  done
fi

if [ -n "${ADMIN_PASSWORD:-}" ]; then
  echo "Creating admin dashboard password Secret in $NS..."
  kubectl -n "$NS" create secret generic claw-admin \
    --from-literal=password="$ADMIN_PASSWORD" --dry-run=client -o yaml | kubectl apply -f -
fi

echo "helm upgrade..."
helm upgrade claw "$ROOT/charts/claw" -n "$NS" --reuse-values --set slack.enabled=true
kubectl -n "$NS" rollout restart statefulset/claw-controller
kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=120s

echo "Done. Watch the connector:"
echo "  kubectl -n $NS logs -f statefulset/claw-controller | grep -i slack"
