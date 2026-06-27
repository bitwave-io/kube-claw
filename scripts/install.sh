#!/usr/bin/env bash
# kube-claw installer. Installs the control plane onto your CURRENT kubectl
# context using the published Docker Hub images — no local image build required.
#
# It prompts for (and stores in Kubernetes Secrets, never in Helm values/history):
#   - Slack Socket Mode tokens   (app-level xapp-… + bot xoxb-…)   [optional]
#   - Anthropic API key          (powers the agent loop + router)  [optional]
#   - Admin dashboard password   (basic-auth for /ui)              [auto-generated if blank]
#   - Public UI base URL         (used in secret-intake links)     [optional]
#
# Usage:
#   ./scripts/install.sh                         # interactive, current kube context
#   IMAGE_TAG=v0.2.0 ./scripts/install.sh        # pin a published tag
#   ./scripts/install.sh --set controller.replicas=2   # extra args pass through to helm
#
# Override the images (e.g. a private mirror or a custom build):
#   IMAGE_REPO=docker.io/myorg/claw-controller \
#   RUNNER_IMAGE=docker.io/myorg/claw-runner:mytag \
#   IMAGE_TAG=mytag ./scripts/install.sh
set -euo pipefail

NS="${NS:-claw-system}"
AGENTS_NS="${AGENTS_NS:-claw-agents}"
IMAGE_REPO="${IMAGE_REPO:-docker.io/bitwavecode/claw-controller}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
RUNNER_IMAGE="${RUNNER_IMAGE:-docker.io/bitwavecode/claw-runner:${IMAGE_TAG}}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# --- preflight ---------------------------------------------------------------
for bin in kubectl helm; do
  command -v "$bin" >/dev/null 2>&1 || { echo "error: '$bin' not found on PATH" >&2; exit 1; }
done
if ! kubectl cluster-info >/dev/null 2>&1; then
  echo "error: kubectl can't reach a cluster. Point your kubeconfig at the target cluster first." >&2
  exit 1
fi

CTX="$(kubectl config current-context 2>/dev/null || echo '?')"
echo "kube-claw will install into:"
echo "  context:    $CTX"
echo "  namespaces: $NS (control plane), $AGENTS_NS (agent run pods)"
echo "  image:      ${IMAGE_REPO}:${IMAGE_TAG}"
echo "  runner:     ${RUNNER_IMAGE}"
echo
read -rp "Proceed? [y/N] " go
[[ "$go" =~ ^[Yy] ]] || { echo "aborted."; exit 0; }

# Apply CRDs with kubectl, not Helm: Helm only installs crds/ on first install and
# never upgrades them, so kubectl apply is the robust path for install AND upgrade.
kubectl apply -f "$ROOT/charts/claw-crds/crds/"
kubectl get ns "$NS"        >/dev/null 2>&1 || kubectl create ns "$NS"
kubectl get ns "$AGENTS_NS" >/dev/null 2>&1 || kubectl create ns "$AGENTS_NS"

# --- Slack (optional) --------------------------------------------------------
slack_args=()
read -rp "Enable the Slack connector? [y/N] " yn
if [[ "$yn" =~ ^[Yy] ]]; then
  read -rsp "  Slack app-level token (xapp-...): " app; echo
  read -rsp "  Slack bot token        (xoxb-...): " bot; echo
  if [[ -z "$app" || -z "$bot" ]]; then
    echo "  both tokens are required; skipping Slack" >&2
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

# --- Anthropic key (optional) ------------------------------------------------
# Injected into every run pod (agent loop) and the controller (LLM image router),
# so it lives in BOTH the agents namespace and the controller namespace.
read -rsp "Anthropic API key for the agent loop (sk-ant-..., blank to skip): " anth; echo
if [[ -n "$anth" ]]; then
  for n in "$AGENTS_NS" "$NS"; do
    kubectl -n "$n" create secret generic claw-anthropic-key \
      --from-literal=api-key="$anth" --dry-run=client -o yaml | kubectl apply -f -
  done
  echo "  stored Anthropic key in Secret claw-anthropic-key (namespaces $AGENTS_NS, $NS)"
fi

# --- Admin dashboard password ------------------------------------------------
# Basic-auth (user "admin") for the /ui admin dashboard. Blank => generate one.
read -rsp "Admin dashboard password (blank to auto-generate): " admin; echo
if [[ -z "$admin" ]]; then
  admin="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24)"
  GENERATED_ADMIN="$admin"
fi
kubectl -n "$NS" create secret generic claw-admin \
  --from-literal=password="$admin" --dry-run=client -o yaml | kubectl apply -f -
echo "  stored admin password in Secret claw-admin (user: admin)"

# --- Public UI base URL (optional) -------------------------------------------
# Used in the one-time secret-intake links the bot DMs. Default suits a local
# port-forward; set the public Ingress host in prod.
ui_args=()
read -rp "Public UI base URL for secret links [http://localhost:8090]: " uiurl
if [[ -n "$uiurl" ]]; then
  ui_args=(--set controller.uiBaseURL="$uiurl")
fi

# --- install -----------------------------------------------------------------
helm upgrade --install claw "$ROOT/charts/claw" -n "$NS" \
  --set image.repository="$IMAGE_REPO" \
  --set image.tag="$IMAGE_TAG" \
  --set controller.runnerImage="$RUNNER_IMAGE" \
  ${slack_args[@]+"${slack_args[@]}"} \
  ${ui_args[@]+"${ui_args[@]}"} "$@"

kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=180s

echo
echo "Done. kube-claw is running in '$NS' on context '$CTX'."
if [[ -n "${GENERATED_ADMIN:-}" ]]; then
  echo "  Admin password (user 'admin'): $GENERATED_ADMIN"
fi
echo
echo "Reach the admin UI:"
echo "  kubectl -n $NS port-forward svc/claw-controller 8090:8090"
echo "  open http://localhost:8090/ui"
echo
echo "If you enabled Slack, add the bot to a channel — it DMs the inviter to set"
echo "up routing. See the README 'Slack app setup' section for required scopes."
