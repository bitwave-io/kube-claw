#!/usr/bin/env bash
# End-to-end test of the self-update plane (DESIGN.md §24) on a THROWAWAY k3d
# cluster. Prod-safe: refuses to run against a non-k3d context.
#
# Phases:
#   A  install the OLD chart (≤0.3.x, from git history) → controller runs,
#      write a marker into the SQLite store
#   B  helm upgrade to the NEW chart → Helm deletes its StatefulSet, the
#      supervisor recreates it, the PVC (and marker) survive (§24.8)
#   C  publish a SIGNED manifest v0.9.1 → auto mode detects, self-approves,
#      applies; controller confirms startup; phase returns to Idle
#   D  publish a TAMPERED manifest → rejected (signature fail-closed), the
#      available version does not move
#   E  publish a signed manifest pointing at a NONEXISTENT image → apply,
#      confirm deadline passes → auto-rollback to v0.9.1 (stuck pod deleted,
#      approval re-pointed at the rollback target — no downgrade to the floor)
#
# Usage: ./scripts/e2e-selfupdate-k3d.sh   (takes ~15-25 min incl. image builds)
#   SKIP_BUILD=1  reuse previously built/imported images
set -euo pipefail

CLUSTER="${CLUSTER:-claw-e2e}"
NS=claw-system
OLD_CHART_REF="${OLD_CHART_REF:-e0b88ea}"   # last pre-supervisor chart
MANIFEST_PORT="${MANIFEST_PORT:-8931}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/claw-e2e.XXXXXX)"
WWW="$WORK/www"
export KUBECONFIG="$WORK/kubeconfig"
mkdir -p "$WWW"

PASS=()
step()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok()    { printf '\033[1;32mPASS\033[0m %s\n' "$*"; PASS+=("$*"); }
fail()  { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; dump_state; exit 1; }

dump_state() {
  echo "--- state dump ---" >&2
  kubectl -n "$NS" get controlplane claw -o yaml 2>/dev/null | sed -n '1,80p' >&2 || true
  kubectl -n "$NS" get pods -o wide >&2 || true
  kubectl -n "$NS" logs deploy/claw-supervisor --tail=40 >&2 || true
}

# wait_for "description" timeout_seconds command...
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local start; start=$(date +%s)
  while true; do
    if "$@" >/dev/null 2>&1; then ok "$desc"; return 0; fi
    if (( $(date +%s) - start > timeout )); then fail "$desc (timed out after ${timeout}s)"; fi
    sleep 5
  done
}

cp_json() { kubectl -n "$NS" get controlplane claw -o json; }
cp_field() { cp_json | python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1], {'d':d}))" "$1"; }
sts_image() { kubectl -n "$NS" get statefulset claw-controller -o jsonpath='{.spec.template.spec.containers[0].image}'; }

# claw_api METHOD PATH [JSON_BODY] — controller API via an in-cluster curl pod.
# Retries inside the pod: k3s's kube-router NetworkPolicy enforcement takes a
# few seconds to add a NEW pod's IP to its allow-ipsets, so the first requests
# from a fresh pod bounce off the controller netpol with connection-refused.
claw_api() {
  local method="$1" path="$2" body="${3:-}"
  local curl_cmd="curl -sS -m 5 -X $method http://claw-controller.${NS}.svc:8443${path}"
  [[ -n "$body" ]] && curl_cmd="$curl_cmd -H 'Content-Type: application/json' -d '$body'"
  kubectl -n "$NS" run "claw-e2e-curl-$RANDOM" --rm -i --restart=Never --quiet \
    --image=curlimages/curl:8.11.1 --command -- sh -c "
      for i in \$(seq 1 12); do
        if out=\$($curl_cmd 2>/dev/null); then echo \"\$out\"; exit 0; fi
        sleep 2
      done
      echo CLAW_API_RETRIES_EXHAUSTED; exit 1"
}

publish_manifest() { # version controller_image runner_image sign(yes/no)
  local ver="$1" ctrl="$2" runner="$3" sign="$4"
  cat > "$WWW/manifest-stable.json" <<EOF
{"schemaVersion":1,"channel":"stable","version":"${ver}",
 "images":{"controller":"${ctrl}","runner":"${runner}"},
 "notes":"e2e ${ver}"}
EOF
  if [[ "$sign" == yes ]]; then
    (cd "$ROOT" && go run ./hack/manifest-sign -key "$WORK/manifest-signing.key" \
      -in "$WWW/manifest-stable.json" -out "$WWW/manifest-stable.json.sig")
  fi
  echo "published manifest ${ver} (signed=${sign})"
}

cleanup() {
  [[ -n "${HTTP_PID:-}" ]] && kill "$HTTP_PID" 2>/dev/null || true
}
trap cleanup EXIT

# --- preflight + cluster -------------------------------------------------------
step "Preflight + fresh k3d cluster '$CLUSTER'"
for bin in docker k3d kubectl helm go python3 git; do
  command -v "$bin" >/dev/null || { echo "missing: $bin" >&2; exit 1; }
done
k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
k3d cluster create "$CLUSTER" --kubeconfig-update-default=false --kubeconfig-switch-context=false --wait
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG"
case "$(kubectl config current-context)" in
  k3d-*) ok "context is k3d ($(kubectl config current-context))" ;;
  *) fail "context is not a k3d cluster" ;;
esac

# --- images --------------------------------------------------------------------
step "Build + import images (v0.9.0 full set, v0.9.1 controller+runner)"
cd "$ROOT"
if [[ -z "${SKIP_BUILD:-}" ]]; then
  docker build -q --build-arg VERSION=v0.9.0 -t kube-claw-controller:v0.9.0 .
  docker build -q --build-arg VERSION=v0.9.0 -f Dockerfile.runner     -t kube-claw-runner:v0.9.0 .
  docker build -q --build-arg VERSION=v0.9.0 -f Dockerfile.supervisor -t kube-claw-supervisor:v0.9.0 .
  docker build -q --build-arg VERSION=v0.9.1 -t kube-claw-controller:v0.9.1 .
  docker build -q --build-arg VERSION=v0.9.1 -f Dockerfile.runner     -t kube-claw-runner:v0.9.1 .
fi
k3d image import kube-claw-controller:v0.9.0 kube-claw-runner:v0.9.0 kube-claw-supervisor:v0.9.0 \
  kube-claw-controller:v0.9.1 kube-claw-runner:v0.9.1 -c "$CLUSTER"
ok "images imported"

# --- signing key + manifest server ----------------------------------------------
step "Signing key + local manifest server (host.k3d.internal:${MANIFEST_PORT})"
go run ./hack/manifest-sign -keygen -dir "$WORK"
publish_manifest v0.9.0 kube-claw-controller:v0.9.0 kube-claw-runner:v0.9.0 yes
(cd "$WWW" && exec python3 -m http.server "$MANIFEST_PORT" --bind 0.0.0.0) >"$WORK/http.log" 2>&1 &
HTTP_PID=$!
sleep 1
curl -sf "http://localhost:${MANIFEST_PORT}/manifest-stable.json" >/dev/null || fail "manifest server up"
ok "manifest server serving"

# --- Phase A: old chart ---------------------------------------------------------
step "Phase A: install the OLD chart ($OLD_CHART_REF) with v0.9.0 images"
git archive "$OLD_CHART_REF" charts | tar -x -C "$WORK"
kubectl apply -f "$WORK/charts/claw/crds/"
kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl create ns claw-agents --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install claw "$WORK/charts/claw" -n "$NS" \
  --set image.repository=kube-claw-controller --set image.tag=v0.9.0 \
  --set image.pullPolicy=IfNotPresent \
  --set controller.runnerImage=kube-claw-runner:v0.9.0
kubectl -n "$NS" rollout status statefulset/claw-controller --timeout=180s
ok "old chart controller is up"

# Marker written into the SQLite store — must survive the chart migration.
claw_api PUT /v1/settings/upgrade-admin '{"value":"U_E2E_MARKER"}' | grep -q U_E2E_MARKER \
  || fail "write store marker"
ok "store marker written"

# --- Phase B: chart migration (old → supervisor-owned) --------------------------
step "Phase B: upgrade to the NEW chart — supervisor adopts, PVC survives (§24.8)"
kubectl apply -f "$ROOT/charts/claw/crds/"
helm upgrade claw "$ROOT/charts/claw" -n "$NS" \
  --set image.repository=kube-claw-controller \
  --set image.runnerRepository=kube-claw-runner \
  --set supervisor.repository=kube-claw-supervisor --set supervisor.tag=v0.9.0 \
  --set image.pullPolicy=IfNotPresent \
  --set version=v0.9.0 \
  --set updates.mode=auto \
  --set updates.checkInterval=1m \
  --set updates.confirmDeadline=3m \
  --set updates.manifestURL="http://host.k3d.internal:${MANIFEST_PORT}/manifest-stable.json" \
  --set-file updates.manifestPublicKey="$WORK/manifest-signing.pub"
kubectl -n "$NS" rollout status deployment/claw-supervisor --timeout=180s
# Helm deletes its old StatefulSet late in the upgrade — possibly AFTER the
# supervisor already recreated one (which Helm then deletes too; the supervisor
# recreates again). Poll readiness instead of a single rollout-status watch.
wait_for "supervisor recreated the controller StatefulSet" 180 \
  kubectl -n "$NS" get statefulset claw-controller
wait_for "controller pod ready under the supervisor-owned StatefulSet" 300 \
  bash -c '[ "$(kubectl -n claw-system get statefulset claw-controller -o jsonpath="{.status.readyReplicas}")" = 1 ]'
wait_for "controller confirmed startup (runningVersion=v0.9.0)" 180 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.runningVersion}")" = v0.9.0 ]'
claw_api GET /v1/settings | grep -q U_E2E_MARKER || fail "store marker survived the migration"
ok "store marker survived the migration (PVC reattached)"

# --- Phase C: signed auto-upgrade -----------------------------------------------
step "Phase C: publish signed v0.9.1 → auto-approve → apply → confirm"
publish_manifest v0.9.1 kube-claw-controller:v0.9.1 kube-claw-runner:v0.9.1 yes
wait_for "poller detected v0.9.1" 180 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.availableVersion}")" = v0.9.1 ]'
wait_for "auto mode approved v0.9.1" 120 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.metadata.annotations.claw\.run/approved-by}")" = auto ]'
wait_for "StatefulSet rolled to v0.9.1" 120 \
  bash -c '[ "$(kubectl -n claw-system get statefulset claw-controller -o jsonpath="{.spec.template.spec.containers[0].image}")" = kube-claw-controller:v0.9.1 ]'
wait_for "new controller confirmed startup (runningVersion=v0.9.1)" 300 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.runningVersion}")" = v0.9.1 ]'
wait_for "phase returned to Idle" 120 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.phase}")" = Idle ]'
claw_api GET /v1/version | grep -q v0.9.1 || fail "API reports v0.9.1"
ok "API reports v0.9.1"
claw_api GET /v1/settings | grep -q U_E2E_MARKER || fail "store marker survived the upgrade"
ok "store marker survived the upgrade"

# --- Phase D: tampered manifest rejected ----------------------------------------
step "Phase D: tampered manifest (stale signature) is rejected fail-closed"
publish_manifest v0.9.2 kube-claw-controller:v0.9.1 kube-claw-runner:v0.9.1 no  # reuses OLD .sig → invalid
sleep 130  # > checkInterval + poll tick
[ "$(kubectl -n "$NS" get controlplane claw -o jsonpath='{.status.availableVersion}')" = v0.9.1 ] \
  || fail "tampered manifest must not move availableVersion"
kubectl -n "$NS" logs deploy/claw-supervisor --tail=200 | grep -q "verification FAILED" \
  || fail "supervisor logged a signature verification failure"
ok "tampered manifest rejected (availableVersion still v0.9.1, verification FAILED logged)"

# --- Phase E: broken release auto-rolls-back ------------------------------------
step "Phase E: signed v0.9.2 with a nonexistent image → deadline → auto-rollback"
publish_manifest v0.9.2 kube-claw-controller:v0.9.2 kube-claw-runner:v0.9.2 yes
wait_for "broken v0.9.2 applied (StatefulSet moved)" 240 \
  bash -c '[ "$(kubectl -n claw-system get statefulset claw-controller -o jsonpath="{.spec.template.spec.containers[0].image}")" = kube-claw-controller:v0.9.2 ]'
# Confirm deadline is 3m; allow scheduling + watchdog laps on top.
wait_for "rollback: StatefulSet back on v0.9.1" 420 \
  bash -c '[ "$(kubectl -n claw-system get statefulset claw-controller -o jsonpath="{.spec.template.spec.containers[0].image}")" = kube-claw-controller:v0.9.1 ]'
wait_for "rollback confirmed (runningVersion=v0.9.1, phase Idle)" 300 \
  bash -c '[ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.phase}")" = Idle ] && [ "$(kubectl -n claw-system get controlplane claw -o jsonpath="{.status.runningVersion}")" = v0.9.1 ]'
[ "$(kubectl -n "$NS" get controlplane claw -o jsonpath='{.status.lastRollback.from}')" = v0.9.2 ] \
  || fail "lastRollback.from records v0.9.2"
ok "lastRollback.from records v0.9.2"
[ "$(kubectl -n "$NS" get controlplane claw -o jsonpath='{.metadata.annotations.claw\.run/approved-version}')" = v0.9.1 ] \
  || fail "approval re-pointed at the rollback target v0.9.1 (no downgrade to the floor)"
[ "$(kubectl -n "$NS" get controlplane claw -o jsonpath='{.metadata.annotations.claw\.run/approved-by}')" = rollback ] \
  || fail "approval re-pointed by 'rollback'"
ok "approval re-pointed at v0.9.1 by rollback (no downgrade to the floor)"
# Give the reconciler time to do the WRONG thing if it were going to (the bug
# this guards: post-rollback downgrade to spec.version).
sleep 45
[ "$(sts_image)" = kube-claw-controller:v0.9.1 ] || fail "held v0.9.1 after rollback (no floor downgrade)"
ok "held v0.9.1 after rollback (no floor downgrade)"
claw_api GET /v1/settings | grep -q U_E2E_MARKER || fail "store marker survived the rollback"
ok "store marker survived the rollback"

# --- summary --------------------------------------------------------------------
step "E2E complete — ${#PASS[@]} assertions passed"
printf '  ✓ %s\n' "${PASS[@]}"
echo
echo "Cluster '$CLUSTER' left running for inspection. Delete with: k3d cluster delete $CLUSTER"
