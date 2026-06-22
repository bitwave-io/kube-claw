#!/usr/bin/env bash
# Interactive kube-claw installer. Helm can't prompt mid-install, so this wrapper
# prompts for the Slack tokens (Socket Mode: app-level xapp-… + bot xoxb-…),
# stores them in a Kubernetes Secret (NOT in Helm values/history), and installs.
#
# Usage:
#   scripts/install.sh                       # prompts; uses image claw-controller:dev
#   IMAGE_REPO=ghcr.io/traego/claw-controller IMAGE_TAG=v0.2.0 scripts/install.sh
set -euo pipefail

NS="${NS:-claw-system}"
AGENTS_NS="${AGENTS_NS:-claw-agents}"
IMAGE_REPO="${IMAGE_REPO:-claw-controller}"
IMAGE_TAG="${IMAGE_TAG:-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "Installing kube-claw into namespace '$NS' (image ${IMAGE_REPO}:${IMAGE_TAG})"

# Apply CRDs with kubectl, not Helm: Helm only installs crds/ on first install and
# never upgrades them, so kubectl apply is the robust path for install AND upgrade.
kubectl apply -f "$ROOT/charts/claw-crds/crds/"
kubectl get ns "$NS"        >/dev/null 2>&1 || kubectl create ns "$NS"
kubectl get ns "$AGENTS_NS" >/dev/null 2>&1 || kubectl create ns "$AGENTS_NS"

slack_args=()
read -rp "Enable the Slack connector? [y/N] " yn
if [[ "$yn" =~ ^[Yy] ]]; then
  read -rsp "  Slack app-level token (xapp-...): " app; echo
  read -rsp "  Slack bot token        (xoxb-...): " bot; echo
  if [[ -z "$app" || -z "$bot" ]]; then
    echo "both tokens are required; skipping Slack" >&2
  else
    kubectl -n "$NS" create secret generic claw-slack-tokens \
      --from-literal=app-token="$app" --from-literal=bot-token="$bot" \
      --dry-run=client -o yaml | kubectl apply -f -
    slack_args=(--set slack.enabled=true)
    echo "  stored tokens in Secret claw-slack-tokens"
    echo "  (no channel to configure — add the bot to any channel and it will"
    echo "   DM the inviter to ask how it should behave there)"
  fi
fi

# Anthropic API key powers the agent loop. Injected into every run pod, so it
# lives in the AGENTS namespace (where run pods read it), keyed "api-key".
read -rsp "Anthropic API key for the agent loop (sk-ant-..., blank to skip): " anth; echo
if [[ -n "$anth" ]]; then
  # agents ns = run pods (agent loop); controller ns = the LLM image router.
  for n in "$AGENTS_NS" "$NS"; do
    kubectl -n "$n" create secret generic claw-anthropic-key \
      --from-literal=api-key="$anth" --dry-run=client -o yaml | kubectl apply -f -
  done
  echo "  stored Anthropic key in Secret claw-anthropic-key (namespaces $AGENTS_NS, $NS)"
fi

helm upgrade --install claw "$ROOT/charts/claw" -n "$NS" \
  --set image.repository="$IMAGE_REPO" \
  --set image.tag="$IMAGE_TAG" \
  --set image.pullPolicy=IfNotPresent \
  ${slack_args[@]+"${slack_args[@]}"} "$@"

kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=120s
echo "Done. kube-claw is running in '$NS'."
