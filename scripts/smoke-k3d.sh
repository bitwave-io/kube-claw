#!/usr/bin/env bash
# Reproducible local smoke test for kube-claw on k3d.
#
# Stands up an isolated k3d cluster, builds + imports the controller and runner
# images, helm-installs the charts, applies the example Agent, then triggers a
# run and asserts the agent responds end-to-end.
#
# SAFETY: uses a dedicated kubeconfig (KUBECONFIG=/tmp/claw-kubeconfig) and never
# touches your default kubeconfig (which may point at prod). It refuses to run
# kubectl/helm against any context whose name does not start with "k3d-".
#
# Usage:
#   scripts/smoke-k3d.sh           # create (if needed), build, deploy, test
#   scripts/smoke-k3d.sh --clean   # delete the cluster and exit
set -euo pipefail

CLUSTER=claw-dev
export KUBECONFIG=/tmp/claw-kubeconfig
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

if [[ "${1:-}" == "--clean" ]]; then
  k3d cluster delete "$CLUSTER"
  exit 0
fi

# 1. Cluster (isolated kubeconfig; do not touch the default/prod one).
if ! k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  k3d cluster create "$CLUSTER" \
    --kubeconfig-update-default=false --kubeconfig-switch-context=false --wait
fi
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG"

CTX="$(kubectl config current-context)"
case "$CTX" in
  k3d-*) echo "context OK: $CTX" ;;
  *) echo "REFUSING: context '$CTX' is not a k3d cluster" >&2; exit 1 ;;
esac

# 2. Build + import images.
docker build -q -t kube-claw-controller:dev .
docker build -q -f Dockerfile.runner -t kube-claw-runner:dev .
k3d image import kube-claw-controller:dev kube-claw-runner:dev -c "$CLUSTER"

# 3. Install/upgrade charts.
helm upgrade --install claw-crds ./charts/claw-crds
kubectl create namespace claw-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace claw-agents --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install claw ./charts/claw -n claw-system \
  --set image.repository=kube-claw-controller --set image.tag=dev --set image.pullPolicy=IfNotPresent \
  --set controller.runnerImage=kube-claw-runner:dev
kubectl -n claw-system rollout restart statefulset/claw-controller
kubectl -n claw-system rollout status statefulset/claw-controller --timeout=120s

# 4. Apply the example Agent.
kubectl apply -f examples/gcp-cost-slack/agent.yaml
sleep 3
kubectl -n claw-agents get agent gcp-cost \
  -o jsonpath='{"agent phase="}{.status.phase}{" digest="}{.status.selectedImageDigest}{"\n"}'

# 5. Full secret loop: create secret → submit value → run blocks → approve →
#    bootstrap attests + materializes → runner uses the secret → Succeeded.
kubectl -n claw-system port-forward svc/claw-controller 18443:8443 >/tmp/claw-pf-api.log 2>&1 &
PFA=$!
kubectl -n claw-system port-forward svc/claw-controller 18090:8090 >/tmp/claw-pf-ui.log 2>&1 &
PFU=$!
trap 'kill $PFA $PFU 2>/dev/null || true' EXIT
sleep 4

api=http://localhost:18443

# Create the secret the agent requires + submit its value via the one-time link.
URL="$(curl -s -X POST "$api/v1/secrets" -H 'content-type: application/json' \
  -d '{"namespace":"claw-agents","name":"gcp-billing-readonly","type":"gcp.serviceAccountKey","granters":["U_ALEX"]}' \
  | sed -n 's/.*"intakeURL":"\([^"]*\)".*/\1/p')"
if [[ -n "$URL" ]]; then
  curl -s -o /dev/null -X POST "$URL" --data-urlencode 'value={"private_key":"demo-key"}'
  echo "secret submitted via one-time link"
fi

RID="$(curl -s -X POST "$api/v1/runs" -H 'content-type: application/json' \
  -d '{"namespace":"claw-agents","agent":"gcp-cost","input":"why did GCP cost spike yesterday?"}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
echo "run: $RID"
sleep 5

# If blocked on approval, approve the pending request (CLI break-glass path).
REQ="$(curl -s "$api/v1/secret-requests?status=Pending" | grep -o '"id":"req-[a-f0-9]*"' | head -1 | sed 's/.*"\(req-[a-f0-9]*\)"/\1/')"
if [[ -n "$REQ" ]]; then
  echo "approving request $REQ"
  curl -s -o /dev/null -X POST "$api/v1/secret-requests/$REQ/approve" -d '{"approver":"smoke","reason":"smoke test"}'
fi

for i in $(seq 1 20); do
  R="$(curl -s "$api/v1/runs/$RID")"
  PHASE="$(echo "$R" | sed -n 's/.*"phase":"\([^"]*\)".*/\1/p')"
  echo "  [$((i*2))s] phase=$PHASE"
  if [[ "$PHASE" == "Succeeded" ]]; then
    echo "SMOKE TEST PASSED"
    echo "$R"
    exit 0
  fi
  if [[ "$PHASE" == "Failed" ]]; then
    echo "SMOKE TEST FAILED: run Failed" >&2
    kubectl -n claw-agents logs -l "claw.run/run-id=$RID" --tail=20 2>&1 || true
    exit 1
  fi
  sleep 2
done
echo "SMOKE TEST FAILED: run did not reach Succeeded" >&2
exit 1
