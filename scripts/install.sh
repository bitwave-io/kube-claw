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
#   VERSION=0.5.0 ./scripts/install.sh           # pin a specific published release
#   ./scripts/install.sh --set controller.replicas=2   # extra args pass through to helm
#
# Override the images (e.g. a private mirror or a custom build). With a custom
# registry, self-update degrades to notify-only unless you also publish your own
# release manifest and set updates.manifestURL (DESIGN.md §24.3):
#   IMAGE_REPO=docker.io/myorg/claw-controller \
#   RUNNER_REPO=docker.io/myorg/claw-runner \
#   SUPERVISOR_REPO=docker.io/myorg/claw-supervisor \
#   VERSION=mytag ./scripts/install.sh
set -euo pipefail

NS="${NS:-claw-system}"
AGENTS_NS="${AGENTS_NS:-claw-agents}"
IMAGE_REPO="${IMAGE_REPO:-docker.io/bitwavecode/kube-claw-controller}"
RUNNER_REPO="${RUNNER_REPO:-docker.io/bitwavecode/kube-claw-runner}"
SUPERVISOR_REPO="${SUPERVISOR_REPO:-docker.io/bitwavecode/kube-claw-supervisor}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# The pinned release version (immutable tag — NOT latest; self-update needs a
# semver-comparable identity). Defaults to the chart's appVersion.
VERSION="${VERSION:-${IMAGE_TAG:-}}"
if [[ -z "$VERSION" ]]; then
  VERSION="$(sed -n 's/^appVersion: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$ROOT/charts/claw/Chart.yaml")"
fi

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
echo "  version:    ${VERSION}"
echo "  images:     ${IMAGE_REPO} + ${RUNNER_REPO} + ${SUPERVISOR_REPO}"
echo
read -rp "Proceed? [y/N] " go
[[ "$go" =~ ^[Yy] ]] || { echo "aborted."; exit 0; }

# Apply CRDs with kubectl, not Helm: Helm only installs crds/ on first install and
# never upgrades them, so kubectl apply is the robust path for install AND upgrade.
kubectl apply -f "$ROOT/charts/claw/crds/"
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
  # This URL is only a link label — install.sh does NOT provision an Ingress or a
  # public IP (it's the portable, any-cluster path; the controller Service stays
  # ClusterIP). A non-localhost URL here resolves nowhere unless you set up
  # ingress separately. Flag it so the host isn't silently a dead link.
  if [[ "$uiurl" != http://localhost* && "$uiurl" != http://127.0.0.1* ]]; then
    echo "  NOTE: install.sh does not set up an Ingress for '$uiurl'. The Service"
    echo "        is ClusterIP (in-cluster only), so there's no IP to point DNS at."
    echo "        For a public HTTPS endpoint + static IP + managed TLS on GKE, use"
    echo "        scripts/deploy-gke.sh. Otherwise reach the UI via port-forward."
  fi
fi

# --- self-update mode ----------------------------------------------------------
# DESIGN.md §24.4: prompt = ask the upgrade admin in Slack before applying a new
# release; auto = apply unprompted (still health-watched); manual = only helm
# moves the version (new releases are announced, never self-applied).
UPDATES_MODE="${UPDATES_MODE:-}"
if [[ -z "$UPDATES_MODE" ]]; then
  read -rp "Self-update mode — prompt/auto/manual [prompt]: " UPDATES_MODE
  UPDATES_MODE="${UPDATES_MODE:-prompt}"
fi
case "$UPDATES_MODE" in
  prompt|auto|manual) ;;
  *) echo "  unknown mode '$UPDATES_MODE', using 'prompt'"; UPDATES_MODE=prompt ;;
esac

# --- install -----------------------------------------------------------------
helm upgrade --install claw "$ROOT/charts/claw" -n "$NS" \
  --set image.repository="$IMAGE_REPO" \
  --set image.runnerRepository="$RUNNER_REPO" \
  --set supervisor.repository="$SUPERVISOR_REPO" \
  --set version="$VERSION" \
  --set updates.mode="$UPDATES_MODE" \
  ${slack_args[@]+"${slack_args[@]}"} \
  ${ui_args[@]+"${ui_args[@]}"} "$@"

# The supervisor creates the controller StatefulSet from the ControlPlane CR
# (it is no longer a Helm-rendered object — DESIGN.md §24.7).
kubectl -n "$NS" rollout status deployment/claw-supervisor --timeout=180s
echo "waiting for the supervisor to create the controller..."
for i in $(seq 1 60); do
  kubectl -n "$NS" get statefulset/claw-controller >/dev/null 2>&1 && break
  sleep 2
done
kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=300s

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
