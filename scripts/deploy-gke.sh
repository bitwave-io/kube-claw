#!/usr/bin/env bash
# Interactive end-to-end kube-claw deploy to GKE (Autopilot-friendly).
#
# Prompts for everything: GCP project/region, Artifact Registry repo + image tag,
# the public UI hostname, the agent secrets (Slack, Anthropic, admin password),
# and whether to provision the GCP-side bits (Artifact Registry repo + a global
# static IP for the Ingress). It then:
#   1. builds + pushes the images to Artifact Registry (scripts/build-push-gke.sh)
#   2. applies the CRD + namespaces
#   3. stores secrets as Kubernetes Secrets (never in Helm values/history)
#   4. helm-installs the control plane with charts/claw/values-gke.yaml
#
# It does NOT create the GKE cluster or your DNS record — point kubectl at the
# target cluster first, and create an A record for the UI host afterwards.
#
# Usage:
#   ./scripts/deploy-gke.sh            # fully interactive
# Non-interactive overrides (any prompt with a matching env var is skipped):
#   PROJECT=... REGION=... REPO=... TAG=... UI_HOST=... ./scripts/deploy-gke.sh
set -euo pipefail

NS="${NS:-claw-system}"
AGENTS_NS="${AGENTS_NS:-claw-agents}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Must match ui.ingress.gke.staticIpName in charts/claw/values-gke.yaml.
STATIC_IP_NAME="${STATIC_IP_NAME:-claw-ui}"

# --- preflight ---------------------------------------------------------------
for bin in gcloud kubectl helm docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "error: '$bin' not found on PATH" >&2; exit 1; }
done
if ! kubectl cluster-info >/dev/null 2>&1; then
  echo "error: kubectl can't reach a cluster. Run:" >&2
  echo "  gcloud container clusters get-credentials CLUSTER --region REGION --project PROJECT" >&2
  exit 1
fi
CTX="$(kubectl config current-context 2>/dev/null || echo '?')"

# --- prompt helpers ----------------------------------------------------------
# ask VAR "prompt" "default"  -> uses $VAR if already set, else prompts (default shown)
ask() {
  local var="$1" prompt="$2" def="${3:-}" cur="${!1:-}" ans
  if [[ -n "$cur" ]]; then return; fi
  if [[ -n "$def" ]]; then read -rp "$prompt [$def]: " ans; ans="${ans:-$def}"
  else read -rp "$prompt: " ans; fi
  printf -v "$var" '%s' "$ans"
}

echo "kube-claw GKE deploy — target cluster context: $CTX"
echo

# --- GCP / image config ------------------------------------------------------
ask PROJECT "GCP project id" "$(gcloud config get-value project 2>/dev/null || true)"
ask REGION  "Artifact Registry / cluster region" "us-central1"
ask REPO    "Artifact Registry repo name" "kube-claw"
ask TAG     "Image tag" "$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo latest)"
ask UI_HOST "Public UI hostname (DNS A record points here)" "claw.example.com"

[[ -n "$PROJECT" ]] || { echo "error: PROJECT is required" >&2; exit 1; }
REGISTRY="${REGION}-docker.pkg.dev/${PROJECT}/${REPO}"

echo
echo "Plan:"
echo "  context:   $CTX"
echo "  registry:  $REGISTRY  (tag: $TAG)"
echo "  UI host:   https://$UI_HOST"
echo "  namespaces: $NS, $AGENTS_NS"
echo
read -rp "Proceed? [y/N] " go
[[ "$go" =~ ^[Yy] ]] || { echo "aborted."; exit 0; }

# --- optional: provision GCP infra -------------------------------------------
read -rp "Create the Artifact Registry repo '$REPO' if missing? [y/N] " mk
if [[ "$mk" =~ ^[Yy] ]]; then
  gcloud artifacts repositories create "$REPO" \
    --project "$PROJECT" --repository-format=docker --location="$REGION" \
    --description="kube-claw images" 2>/dev/null \
    && echo "  created repo $REPO" || echo "  repo $REPO already exists (or create skipped)"
  gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
fi

read -rp "Reserve a global static IP named '$STATIC_IP_NAME' for the Ingress? [y/N] " mkip
if [[ "$mkip" =~ ^[Yy] ]]; then
  gcloud compute addresses create "$STATIC_IP_NAME" --global --project "$PROJECT" 2>/dev/null \
    && echo "  created static IP $STATIC_IP_NAME" || echo "  static IP $STATIC_IP_NAME already exists"
  IP="$(gcloud compute addresses describe "$STATIC_IP_NAME" --global --project "$PROJECT" \
    --format='value(address)' 2>/dev/null || true)"
  [[ -n "$IP" ]] && echo "  >>> point $UI_HOST DNS A record at: $IP"
fi

# --- build + push images -----------------------------------------------------
read -rp "Build + push images to $REGISTRY now? [Y/n] " bp
if [[ ! "$bp" =~ ^[Nn] ]]; then
  PROJECT="$PROJECT" REGION="$REGION" REPO="$REPO" TAG="$TAG" "$ROOT/scripts/build-push-gke.sh"
fi

# --- CRD + namespaces --------------------------------------------------------
# Apply CRDs with kubectl (Helm never upgrades crds/), then ensure namespaces.
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
    slack_args=(--set slack.enabled=false)
  else
    kubectl -n "$NS" create secret generic claw-slack-tokens \
      --from-literal=app-token="$app" --from-literal=bot-token="$bot" \
      --dry-run=client -o yaml | kubectl apply -f -
    slack_args=(--set slack.enabled=true)
    echo "  stored tokens in Secret claw-slack-tokens"
  fi
else
  # values-gke.yaml defaults slack.enabled=true; turn it off if not configured.
  slack_args=(--set slack.enabled=false)
fi

# --- Anthropic key (optional) ------------------------------------------------
# Injected into every run pod (agent loop) and the controller (LLM image router).
read -rsp "Anthropic API key (sk-ant-..., blank to skip): " anth; echo
if [[ -n "$anth" ]]; then
  for n in "$AGENTS_NS" "$NS"; do
    kubectl -n "$n" create secret generic claw-anthropic-key \
      --from-literal=api-key="$anth" --dry-run=client -o yaml | kubectl apply -f -
  done
  echo "  stored Anthropic key in Secret claw-anthropic-key (namespaces $AGENTS_NS, $NS)"
fi

# --- Admin dashboard password ------------------------------------------------
read -rsp "Admin dashboard password (blank to auto-generate): " admin; echo
if [[ -z "$admin" ]]; then
  admin="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24)"
  GENERATED_ADMIN="$admin"
fi
kubectl -n "$NS" create secret generic claw-admin \
  --from-literal=password="$admin" --dry-run=client -o yaml | kubectl apply -f -
echo "  stored admin password in Secret claw-admin (user: admin)"

# --- install -----------------------------------------------------------------
helm upgrade --install claw "$ROOT/charts/claw" -n "$NS" \
  -f "$ROOT/charts/claw/values-gke.yaml" \
  --set image.repository="${REGISTRY}/kube-claw-controller" \
  --set image.tag="$TAG" \
  --set controller.runnerImage="${REGISTRY}/kube-claw-runner:${TAG}" \
  --set controller.uiBaseURL="https://${UI_HOST}" \
  --set ui.ingress.host="$UI_HOST" \
  ${slack_args[@]+"${slack_args[@]}"} "$@"

kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=180s

echo
echo "Done. kube-claw is deployed to '$NS' on context '$CTX'."
if [[ -n "${GENERATED_ADMIN:-}" ]]; then
  echo "  Admin password (user 'admin'): $GENERATED_ADMIN"
fi
echo
echo "Next:"
echo "  - Point $UI_HOST DNS A record at the static IP (gcloud compute addresses describe $STATIC_IP_NAME --global)."
echo "  - Watch the Ingress + managed cert (Status -> Active can take ~15 min):"
echo "      kubectl -n $NS get ingress"
echo "      kubectl -n $NS describe managedcertificate"
echo "  - Then open https://$UI_HOST/ui (basic auth: admin / your password)."
echo "  - Register a base image + agent:"
echo "      kubectl -n $NS port-forward svc/claw-controller 8443:8443 &"
echo "      export CLAW_CONTROLLER_URL=http://localhost:8443"
echo "      claw baseimage create gcloud --image ${REGISTRY}/kube-claw-gcloud:${TAG} \\"
echo "        --description \"Google Cloud SDK (gcloud, bq)\""
